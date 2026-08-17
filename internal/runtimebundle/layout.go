package runtimebundle

const (
	// MountPath is the container-visible, read-only runtime mount. Keeping it
	// directly below / prevents a workload from replacing it by renaming a
	// writable parent directory.
	MountPath = "/vaka"

	// ImageSubpath is the runtime bundle's directory inside the source image.
	// It is intentionally independent from MountPath: Docker mounts this image
	// subpath at the direct-root container path above.
	ImageSubpath = "opt/vaka"

	InitPath = MountPath + "/sbin/vaka-init"
	NftPath  = MountPath + "/sbin/nft"
)
