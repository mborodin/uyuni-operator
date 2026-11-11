package git

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// go-git does not run the Git LFS smudge filter, so LFS-tracked files are
// checked out as their pointer files. resolveLFSPointers swaps those for the
// real objects through the repository's LFS batch API
// (https://github.com/git-lfs/git-lfs/blob/main/docs/api/batch.md).

const (
	lfsPointerVersion = "version https://git-lfs.github.com/spec/v1"
	// Pointer files are a few lines long; anything bigger is real content.
	lfsPointerMaxSize = 1024
	lfsBatchSize      = 100
	lfsMediaType      = "application/vnd.git-lfs+json"
)

var (
	lfsAPIClient = &http.Client{Timeout: 2 * time.Minute}
	// Vendor RPMs can be a few hundred MB.
	lfsDownloadClient = &http.Client{Timeout: 30 * time.Minute}
)

type lfsObject struct {
	OID  string `json:"oid"`
	Size int64  `json:"size"`
}

type lfsBatchRequest struct {
	Operation string      `json:"operation"`
	Transfers []string    `json:"transfers"`
	Objects   []lfsObject `json:"objects"`
}

type lfsAction struct {
	Href   string            `json:"href"`
	Header map[string]string `json:"header"`
}

type lfsBatchResponse struct {
	Objects []struct {
		OID     string `json:"oid"`
		Size    int64  `json:"size"`
		Actions struct {
			Download *lfsAction `json:"download"`
		} `json:"actions"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	} `json:"objects"`
}

// parseLFSPointer reports whether content is a Git LFS pointer file and, if so,
// which object it points to.
func parseLFSPointer(content []byte) (lfsObject, bool) {
	if len(content) > lfsPointerMaxSize || !bytes.HasPrefix(content, []byte(lfsPointerVersion+"\n")) {
		return lfsObject{}, false
	}
	var obj lfsObject
	hasSize := false
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		key, value, _ := strings.Cut(scanner.Text(), " ")
		switch key {
		case "oid":
			oid, ok := strings.CutPrefix(value, "sha256:")
			if !ok || len(oid) != sha256.Size*2 {
				return lfsObject{}, false
			}
			obj.OID = oid
		case "size":
			size, err := strconv.ParseInt(value, 10, 64)
			if err != nil || size < 0 {
				return lfsObject{}, false
			}
			obj.Size = size
			hasSize = true
		}
	}
	if obj.OID == "" || !hasSize {
		return lfsObject{}, false
	}
	return obj, true
}

// lfsEndpoint derives the LFS server URL from the clone URL the way git-lfs does
// by default: <repository>.git/info/lfs.
func lfsEndpoint(cloneURL string) string {
	endpoint := strings.TrimSuffix(cloneURL, "/")
	if !strings.HasSuffix(endpoint, ".git") {
		endpoint += ".git"
	}
	return endpoint + "/info/lfs"
}

// resolveLFSPointers replaces every LFS pointer file under root with the object
// it points to. Hidden files and directories are skipped, as Clone does not
// read them. username/password are the clone's credentials, reused for the LFS
// API; both empty means anonymous access.
func resolveLFSPointers(root, cloneURL, username, password string) error {
	paths := map[string][]string{} // oid -> pointer files (one object may sit under several names)
	var objects []lfsObject
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path != root && len(d.Name()) > 0 && d.Name()[0] == '.' {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > lfsPointerMaxSize {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		obj, ok := parseLFSPointer(content)
		if !ok {
			return nil
		}
		if _, seen := paths[obj.OID]; !seen {
			objects = append(objects, obj)
		}
		paths[obj.OID] = append(paths[obj.OID], path)
		return nil
	})
	if err != nil {
		return fmt.Errorf("scanning for LFS pointers: %w", err)
	}
	if len(objects) == 0 {
		return nil
	}

	endpoint := lfsEndpoint(cloneURL)
	for start := 0; start < len(objects); start += lfsBatchSize {
		batch := objects[start:min(start+lfsBatchSize, len(objects))]
		resp, err := lfsBatch(endpoint, username, password, batch)
		if err != nil {
			return err
		}
		resolved := map[string]bool{}
		for _, o := range resp.Objects {
			files, wanted := paths[o.OID]
			if !wanted {
				continue
			}
			if o.Error != nil {
				return fmt.Errorf("LFS object %s (%s): %d %s", o.OID, files[0], o.Error.Code, o.Error.Message)
			}
			if o.Actions.Download == nil {
				return fmt.Errorf("LFS object %s (%s): server returned no download action", o.OID, files[0])
			}
			obj := lfsObject{OID: o.OID, Size: o.Size}
			if err := lfsDownload(o.Actions.Download, obj, endpoint, username, password, files[0]); err != nil {
				return fmt.Errorf("LFS object %s (%s): %w", o.OID, files[0], err)
			}
			for _, dup := range files[1:] {
				if err := copyFile(files[0], dup); err != nil {
					return fmt.Errorf("LFS object %s (%s): %w", o.OID, dup, err)
				}
			}
			resolved[o.OID] = true
		}
		for _, obj := range batch {
			if !resolved[obj.OID] {
				return fmt.Errorf("LFS object %s (%s): missing from the batch response", obj.OID, paths[obj.OID][0])
			}
		}
	}
	return nil
}

// lfsBatch asks the LFS server where to download objects from.
func lfsBatch(endpoint, username, password string, objects []lfsObject) (*lfsBatchResponse, error) {
	body, err := json.Marshal(lfsBatchRequest{
		Operation: "download",
		Transfers: []string{"basic"},
		Objects:   objects,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, endpoint+"/objects/batch", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", lfsMediaType)
	req.Header.Set("Content-Type", lfsMediaType)
	if username != "" || password != "" {
		req.SetBasicAuth(username, password)
	}
	resp, err := lfsAPIClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("LFS batch request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("LFS batch request: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var out lfsBatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding LFS batch response: %w", err)
	}
	return &out, nil
}

// lfsDownload streams one object over the pointer file at dst, checking size and
// sha256 against the pointer before replacing it.
func lfsDownload(action *lfsAction, obj lfsObject, endpoint, username, password, dst string) error {
	req, err := http.NewRequest(http.MethodGet, action.Href, nil)
	if err != nil {
		return err
	}
	for k, v := range action.Header {
		req.Header.Set(k, v)
	}
	// GitHub hands out pre-signed storage URLs that need no credentials; a
	// server serving objects from its own host may expect the clone's.
	if req.Header.Get("Authorization") == "" && (username != "" || password != "") && sameHost(action.Href, endpoint) {
		req.SetBasicAuth(username, password)
	}
	resp, err := lfsDownloadClient.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: %s", resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".lfs-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once renamed into place
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, sum), resp.Body)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if n != obj.Size || hex.EncodeToString(sum.Sum(nil)) != obj.OID {
		return fmt.Errorf("downloaded content does not match the pointer (%d bytes, expected %d)", n, obj.Size)
	}
	return os.Rename(tmp.Name(), dst)
}

func sameHost(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	return errA == nil && errB == nil && ua.Host == ub.Host
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
