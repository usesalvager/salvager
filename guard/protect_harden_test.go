package guard

import "testing"

// TestHarden_DefaultLookalikes is the false-positive guard for the tuned default set:
// the real secret is protected, the routine source/template look-alike right next to
// it is NOT. A false deny here is what makes a user turn the hook off, so each pair
// pins one narrowing (.env.* templates carved out; id_* replaced by exact key names).
func TestHarden_DefaultLookalikes(t *testing.T) {
	cases := []struct {
		path string
		hit  bool
	}{
		// .env: real dotenv secrets protected …
		{".env", true},
		{".env.local", true},
		{".env.production", true},
		{".env.staging.local", true}, // .env.*.local
		{"config/.env", true},
		// … but the known-safe, committed templates are not.
		{".env.example", false},
		{".env.sample", false},
		{".env.template", false},
		{".env.dist", false},
		{"config/.env.example", false}, // carved out at any depth

		// SSH keys: the private keys by exact name are protected …
		{"id_rsa", true},
		{"id_dsa", true},
		{"id_ecdsa", true},
		{"id_ed25519", true},
		{".ssh/id_rsa", true},
		// … but ordinary id_-prefixed source files are not (the old id_* bug) …
		{"id_helper.go", false},
		{"id_map.py", false},
		{"id_generator.ts", false},
		{"id_utils.rs", false},
		// … and a public key is not a secret.
		{"id_rsa.pub", false},
		{"id_ed25519.pub", false},
	}
	for _, c := range cases {
		if hit, _ := ProtectedHit(c.path, root); hit != c.hit {
			t.Errorf("ProtectedHit(%q) = %v, want %v", c.path, hit, c.hit)
		}
	}
}
