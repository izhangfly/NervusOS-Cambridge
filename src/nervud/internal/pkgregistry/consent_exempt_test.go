package pkgregistry

import (
	"testing"

	"github.com/nervus-os/nervud/internal/identity"
)

// The criterion for exemption from runtime consent is "shipped with the read-only system image" AND "platform trusted"; both are needed.
//
// Looking only at Source: a low-trust package slipped into the image directory would get through.
// Looking only at Trust: a dynamically installed platform-signed package would also skip the question, while the reason "the user
// accepted it when setting up the machine" only holds for the batch shipped with the image.
func TestProjectGrants_ConsentExemptRequiresBothSourceAndTrust(t *testing.T) {
	cases := []struct {
		name   string
		source Source
		trust  identity.TrustProfile
		want   bool
	}{
		{"system image + platform trust", SourceSystemImage, identity.TrustPlatform, true},
		{"system image + OEM trust", SourceSystemImage, identity.TrustOEM, false},
		{"system image + ordinary trust", SourceSystemImage, identity.TrustOrdinary, false},
		{"dynamic install + platform trust", SourceDynamicInstall, identity.TrustPlatform, false},
		{"dynamic install + ordinary trust", SourceDynamicInstall, identity.TrustOrdinary, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			entry := Entry{
				Manifest:           Manifest{PackageID: "com.example.app"},
				Source:             c.source,
				Trust:              c.trust,
				GrantedPermissions: []string{"perm.storage.user"},
			}
			got := projectGrants([]Entry{entry})
			if len(got) != 1 {
				t.Fatalf("projection count = %d, want 1", len(got))
			}
			if got[0].ConsentExempt != c.want {
				t.Errorf("ConsentExempt = %v, want %v", got[0].ConsentExempt, c.want)
			}
			// Whether exempt or not, the install-time set mustn't change
			if len(got[0].Permissions) != 1 {
				t.Errorf("the permission set was changed: %v", got[0].Permissions)
			}
		})
	}
}
