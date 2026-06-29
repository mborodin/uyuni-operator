package git

import (
	"fmt"
	"os"
	"path/filepath"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// Client handles git repository operations
type Client interface {
	Clone(url, ref, subPath string) (map[string]string, string, error)
}

type gitClient struct{}

// New creates a new Git client
func New() Client {
	return &gitClient{}
}

// Clone clones a git repository and returns all files from subPath
// Returns: map[filePath]fileContents, repoHash, error
func (g *gitClient) Clone(url, ref, subPath string) (map[string]string, string, error) {
	// Create temporary directory
	dir, err := os.MkdirTemp("", "uyuni-repo-*")
	if err != nil {
		return nil, "", fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	// Clone repository
	repo, err := git.PlainClone(dir, false, &git.CloneOptions{
		URL: url,
	})
	if err != nil {
		return nil, "", fmt.Errorf("failed to clone repository: %w", err)
	}

	// Get working tree
	w, err := repo.Worktree()
	if err != nil {
		return nil, "", fmt.Errorf("failed to get worktree: %w", err)
	}

	// Checkout the specified ref (branch/tag)
	if ref != "" {
		refName := plumbing.ReferenceName(fmt.Sprintf("refs/heads/%s", ref))
		err = w.Checkout(&git.CheckoutOptions{
			Branch: refName,
		})
		if err != nil {
			// Try as tag if branch doesn't exist
			refName = plumbing.ReferenceName(fmt.Sprintf("refs/tags/%s", ref))
			err = w.Checkout(&git.CheckoutOptions{
				Branch: refName,
			})
			if err != nil {
				return nil, "", fmt.Errorf("failed to checkout ref %s: %w", ref, err)
			}
		}
	}

	// Get current commit hash
	head, err := repo.Head()
	if err != nil {
		return nil, "", fmt.Errorf("failed to get HEAD: %w", err)
	}
	repoHash := head.Hash().String()

	// Walk through files in subPath
	files := make(map[string]string)
	targetPath := filepath.Join(dir, subPath)

	// Check if subPath exists
	if _, err := os.Stat(targetPath); os.IsNotExist(err) {
		return files, repoHash, nil // Empty directory is not an error
	}

	// Walk directory tree
	err = filepath.WalkDir(targetPath, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		// Skip directories and hidden files
		if d.IsDir() || (len(d.Name()) > 0 && d.Name()[0] == '.') {
			return nil
		}

		// Read file content
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("failed to read file %s: %w", path, readErr)
		}

		// Get relative path from targetPath
		relPath, relErr := filepath.Rel(targetPath, path)
		if relErr != nil {
			return relErr
		}

		// Normalize path separators to forward slashes
		relPath = filepath.ToSlash(relPath)

		// Store with leading slash for Uyuni compatibility
		files["/"+relPath] = string(content)

		return nil
	})

	if err != nil {
		return nil, "", fmt.Errorf("failed to walk directory: %w", err)
	}

	return files, repoHash, nil
}
