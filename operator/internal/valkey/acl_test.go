package valkey_test

import (
	"strings"
	"testing"

	"github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

func TestInitialACLStartsWithAppDisabled(t *testing.T) {
	acl := string(valkey.InitialACL(strings.Repeat("ab", 32), "operator", "replica", "health"))

	for _, line := range []string{
		"user default off\n",
		"user app off #" + strings.Repeat("ab", 32),
		"user operator on >operator ~* &* +@all\n",
		"user replica on >replica -@all +psync +replconf +ping\n",
		"user health on >health -@all +ping +role +info\n",
	} {
		if !strings.Contains(acl, line) {
			t.Errorf("ACL не содержит %q", line)
		}
	}
	if strings.Contains(acl, "user app on") {
		t.Fatal("учётная запись app включена")
	}
}
