package authn_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/truvity/access-roster/identity"

	"github.com/truvity/gemaal/pkg/authn"
)

// stubIssuer scripts access-issuer verification.
type stubIssuer struct {
	who      identity.Verified
	err      error
	verified []string
}

func (s *stubIssuer) Verify(_ context.Context, token string) (identity.Verified, error) {
	s.verified = append(s.verified, token)

	return s.who, s.err
}

func TestNoTokenRefused(t *testing.T) {
	auth := &authn.Authenticator{Issuer: &stubIssuer{}}

	_, err := auth.Authenticate(context.Background(), "")
	require.ErrorIs(t, err, authn.ErrNoCredentials)
}

func TestIssuerVerifiedPerson(t *testing.T) {
	issuer := &stubIssuer{who: identity.Verified{
		Subject: "j.doe@example.com",
		Email:   "j.doe@example.com",
		Name:    "Jane Doe",
		Groups:  []string{"emp:jdoe", "devel:k8s:viewer"},
	}}
	auth := &authn.Authenticator{Reviewer: &stubReviewer{}, Issuer: issuer}

	who, err := auth.Authenticate(context.Background(), "person-token")
	require.NoError(t, err)
	assert.Equal(t, authn.Identity{
		Subject: "j.doe@example.com",
		Email:   "j.doe@example.com",
		Name:    "Jane Doe",
		Groups:  []string{"emp:jdoe", "devel:k8s:viewer"},
		Method:  authn.MethodIssuer,
	}, who)
	assert.Equal(t, []string{"person-token"}, issuer.verified)
}

// The property the switch exists for: a well-formed token whose claims say
// exactly the right things, but which the issuer does not vouch for, is
// refused. The old path decoded claims without checking the signature.
func TestUnverifiedTokenRefused(t *testing.T) {
	issuer := &stubIssuer{err: fmt.Errorf("%w: bad signature", identity.ErrUnverified)}
	auth := &authn.Authenticator{Reviewer: &stubReviewer{}, Issuer: issuer}

	forged := jwt(t, map[string]any{"sub": "root@example.com", "email": "root@example.com", "groups": []any{"devel:k8s:admin"}})

	_, err := auth.Authenticate(context.Background(), forged)
	require.ErrorIs(t, err, identity.ErrUnverified)
}

func TestNoIssuerRefusesWhatTheClusterDoesNotKnow(t *testing.T) {
	auth := &authn.Authenticator{Reviewer: &stubReviewer{}}

	_, err := auth.Authenticate(context.Background(), jwt(t, map[string]any{"email": "a@b.c"}))
	require.ErrorContains(t, err, "no issuer is configured")
}

func TestIssuerUnreachableFails(t *testing.T) {
	auth := &authn.Authenticator{Issuer: &stubIssuer{err: errors.New("discover: connection refused")}}

	_, err := auth.Authenticate(context.Background(), "person-token")
	require.ErrorContains(t, err, "connection refused")
}
