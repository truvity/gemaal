package service

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/truvity/gemaal/pkg/authn"
	"github.com/truvity/gemaal/pkg/config"
)

func TestIsAdminUsers(t *testing.T) {
	t.Parallel()

	s := New(Deps{Config: &config.Config{Authz: config.Authz{
		AdminGroups: []string{"platform:admin"},
		AdminUsers:  []string{"O.Tsarev@Example.com", "379486258717560197"},
	}}})

	cases := []struct {
		name   string
		caller authn.Identity
		want   bool
	}{
		{"admin group still works", authn.Identity{Groups: []string{"platform:admin"}}, true},
		{"email match, case-insensitive", authn.Identity{Subject: "x", Email: "o.tsarev@example.com"}, true},
		{"bare gateway subject match", authn.Identity{Subject: "379486258717560197"}, true},
		{"no match", authn.Identity{Subject: "999", Email: "someone@else.com"}, false},
		{"empty identity", authn.Identity{}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, s.isAdmin(tc.caller))
		})
	}
}
