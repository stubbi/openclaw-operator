/*
Copyright 2026 Paperclip Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package webhook

import (
	"encoding/base64"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/Masterminds/semver/v3"

	openclawv1alpha1 "github.com/paperclipinc/openclaw-operator/api/v1alpha1"
)

var verifiedPluginPackagePattern = regexp.MustCompile(`^(@[a-z0-9][a-z0-9._-]*/)?[a-z0-9][a-z0-9._-]*$`)

func validateVerifiedPlugins(spec *openclawv1alpha1.OpenClawInstanceSpec) error {
	if len(spec.VerifiedPlugins) > 0 && len(spec.Plugins) > 0 {
		return fmt.Errorf("plugins and verifiedPlugins are mutually exclusive")
	}
	if len(spec.VerifiedPlugins) > 20 {
		return fmt.Errorf("verifiedPlugins must contain at most 20 entries")
	}
	basenames := make(map[string]bool)
	for i, pin := range spec.VerifiedPlugins {
		prefix := fmt.Sprintf("verifiedPlugins[%d]", i)
		if len(pin.Package) > 214 || !verifiedPluginPackagePattern.MatchString(pin.Package) {
			return fmt.Errorf("%s.package must be an npm package name without a source prefix or version", prefix)
		}
		if _, err := semver.StrictNewVersion(pin.Version); err != nil || len(pin.Version) > 128 {
			return fmt.Errorf("%s.version must be an exact SemVer version", prefix)
		}
		digest, err := base64.StdEncoding.Strict().DecodeString(strings.TrimPrefix(pin.Integrity, "sha512-"))
		if err != nil || len(digest) != 64 || "sha512-"+base64.StdEncoding.EncodeToString(digest) != pin.Integrity {
			return fmt.Errorf("%s.integrity must be a canonical SHA-512 SRI digest", prefix)
		}
		basename := path.Base(pin.Package)
		if basenames[basename] {
			return fmt.Errorf("%s.package duplicates plugin install directory %q", prefix, basename)
		}
		basenames[basename] = true
	}
	return nil
}
