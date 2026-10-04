package v1alpha1

// VerifiedPluginSpec identifies a reviewed plugin artifact on registry.npmjs.org.
// Integrity covers the package archive, not its transitive dependencies or files
// subsequently modified on the persistent volume. Consent is not a sandbox.
type VerifiedPluginSpec struct {
	// Package is an npm package name without a source prefix or version suffix.
	// +kubebuilder:validation:MaxLength=214
	// +kubebuilder:validation:Pattern=`^(@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*$`
	Package string `json:"package"`

	// Version is an exact SemVer version, including optional prerelease/build
	// identifiers. Tags, ranges and a leading v are not accepted.
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`
	Version string `json:"version"`

	// Integrity is the canonical SHA-512 SRI digest committed alongside Version.
	// Both npm dist.integrity and the downloaded archive must match this value.
	// +kubebuilder:validation:Pattern=`^sha512-[A-Za-z0-9+/]{85}[AQgw]==$`
	Integrity string `json:"integrity"`

	// AcceptCapabilities explicitly consents to the pinned artifact's declared
	// OpenClaw capabilities after verification. Defaults to false; installs that
	// require consent fail without it. Review capabilities whenever pins change.
	// +optional
	AcceptCapabilities bool `json:"acceptCapabilities,omitempty"`
}
