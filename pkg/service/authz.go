package service

import (
	"fmt"
	"strings"

	"connectrpc.com/connect"

	"github.com/truvity/gemaal/pkg/authn"
)

// Authorization is deliberately small and structural:
//
//   - Checkout/Extend touch ONE tenant's lifetime. The caller must own
//     the namespace — their emp:{slug} group (or their resolved email)
//     must render to it through the personal-namespace template — or
//     hold an admin group.
//   - Sweep executes deletions. Admin group only.
//
// There is no per-tenant ACL and no role model: the tenancy design says
// a standing namespace has exactly one owner, and everything else is an
// operator action.

// isAdmin reports whether the caller holds any configured admin group
// or is a configured admin user (subject or email, case-insensitive —
// the rail for gateway sessions whose tokens carry no groups). Nothing
// configured means NOBODY is an admin — the safe default.
func (s *Service) isAdmin(caller authn.Identity) bool {
	if caller.InAny(s.deps.Config.Authz.AdminGroups) {
		return true
	}

	for _, u := range s.deps.Config.Authz.AdminUsers {
		if u == "" {
			continue
		}

		if strings.EqualFold(u, caller.Subject) || (caller.Email != "" && strings.EqualFold(u, caller.Email)) {
			return true
		}
	}

	return false
}

// callerSlugs collects every slug the caller can prove: the prefixed
// token groups ("emp:{slug}") and the resolved email, when the identity
// map knows it.
func (s *Service) callerSlugs(caller authn.Identity) []string {
	var slugs []string

	prefix := s.deps.Config.Authz.GroupPrefix
	for _, group := range caller.Groups {
		if slug, ok := strings.CutPrefix(group, prefix); ok && slug != "" {
			slugs = append(slugs, slug)
		}
	}

	if caller.Email != "" {
		for email, slug := range s.deps.Config.Identity.Emails {
			if strings.EqualFold(email, caller.Email) {
				slugs = append(slugs, slug)

				break
			}
		}
	}

	return slugs
}

// authorizeHold decides Checkout/Extend: the caller must own the
// namespace or be an admin.
func (s *Service) authorizeHold(caller authn.Identity, namespace string) error {
	if s.isAdmin(caller) {
		return nil
	}

	for _, slug := range s.callerSlugs(caller) {
		if s.deps.Config.PersonalNamespace(slug) == namespace {
			return nil
		}
	}

	return connect.NewError(connect.CodePermissionDenied,
		fmt.Errorf("caller %s does not own namespace %s and holds no admin group", caller.Subject, namespace))
}

// authorizeSweep decides Sweep: admin only.
func (s *Service) authorizeSweep(caller authn.Identity) error {
	if s.isAdmin(caller) {
		return nil
	}

	return connect.NewError(connect.CodePermissionDenied,
		fmt.Errorf("caller %s holds no admin group — sweep is admin-only", caller.Subject))
}
