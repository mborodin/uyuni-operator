package v1alpha1

// Group is the API group for all annotations and finalizers managed by this
// operator. Centralized here so a future rename touches one constant.
const Group = "uyuni.uyuni-project.org"

// Annotations recognized on user-managed CRs.
//
// Convention: annotation values are interpreted leniently for "absent" but
// strictly for "present" — only the literal string "true" enables the flag.
// Webhook validators enforce this.
const (
	// AnnForceDelete skips Uyuni-side cleanup during a CR's finalizer run.
	// The CR is removed; whatever existed in Uyuni stays. Used when Uyuni is
	// unreachable or the operator-managed resource needs to be abandoned.
	AnnForceDelete = Group + "/force-delete"

	// AnnRerun on a Task triggers a new run with the same spec. The
	// reconciler strips the annotation after recording the run in status.
	AnnRerun = Group + "/rerun"

	// AnnBuildNow on an ImageProfile triggers a one-off build regardless of
	// the configured BuildPolicy. Stripped after the build is scheduled.
	AnnBuildNow = Group + "/build-now"

	// AnnSyncNow on a SoftwareChannel triggers a one-off repo sync.
	// Stripped after the sync is submitted.
	AnnSyncNow = Group + "/sync-now"

	// AnnBuildVersion on an ImageProfile overrides the auto-generated
	// version string for the next build. Not stripped (lets the customer
	// pin a version for a sequence of builds).
	AnnBuildVersion = Group + "/build-version"

	// AnnReinstallNow on a System triggers a new system.provisionSystem call,
	// reprovisioning a live registered system via its autoinstall profile.
	// The reconciler strips the annotation after recording the action ID in status.
	// Requires spec.autoinstall to be set. Value must be exactly "true".
	AnnReinstallNow = Group + "/reinstall-now"
)
