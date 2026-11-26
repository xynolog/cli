package launcher

// signins.go — the sign-in store, `xynolog login`'s sessions by stack key, in
// the launcher's state home: the Go SDK's store (xynolog.SignInStore,
// protocol/specs/src/sign-in-store), which every program that keeps a sign-in there
// shares, under its lock.

import (
	"context"
	"errors"
	"path/filepath"

	xynolog "github.com/xynolog/sdk/go"
	"github.com/xynolog/sdk/go/api"
)

// tokensPath is the sign-in store's file; "" when there is no state home.
func tokensPath() string {
	dir := StateDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, xynolog.SignInFile)
}

// signInStore is the store the launcher keeps its sign-ins in; false when
// there is no state home to keep it in.
func signInStore() (*xynolog.SignInStore, bool) {
	path := tokensPath()
	if path == "" {
		return nil, false
	}
	return xynolog.NewSignInStore(path), true
}

// LoadTokens is every sign-in the store holds, by stack key: none when there
// is no store, or it cannot be read.
func LoadTokens() map[string]xynolog.SignIn {
	store, ok := signInStore()
	if !ok {
		return map[string]xynolog.SignIn{}
	}
	signIns, err := store.SignIns()
	if err != nil {
		return map[string]xynolog.SignIn{}
	}
	for _, s := range signIns {
		noteSecret(s.Token)
		noteSecret(s.RefreshToken)
	}
	return signIns
}

// keepSignIn keeps the sign-in of the stack key. The store keeps the state
// home to its owner before it writes, with everything created in it from then
// on: the store's file, its lock, and what a renewal writes in their place.
func keepSignIn(ctx context.Context, key string, held api.StoredSession) error {
	store, ok := signInStore()
	if !ok {
		return errors.New("there is no state home to keep the session in")
	}
	return store.Entry(key).Keep(ctx, held)
}
