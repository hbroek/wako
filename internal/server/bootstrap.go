package server

import (
	"errors"

	"github.com/hbroek/wako/internal/auth"
	"github.com/hbroek/wako/internal/store"
)

// Bootstrap makes sure the wiki has an administrator. If it has none, it
// creates one called name; the password is the one given, or a random one,
// returned in generated so the caller can show it once.
func Bootstrap(st *store.Store, name, password string) (created bool, generated string, err error) {
	if n, err := st.CountAdmins(); err != nil || n > 0 {
		return false, "", err
	}
	if !auth.ValidUsername(name) {
		return false, "", errors.New("the administrator name uses letters, digits, '.', '-' and '_' and starts with a letter or digit")
	}
	if password == "" {
		password = auth.RandomToken(12)
		generated = password
	}
	if err := auth.ValidPassword(password); err != nil {
		return false, "", err
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return false, "", err
	}
	// The name may already belong to an ordinary user: promote them.
	if u, err := st.GetUserByName(name); err == nil {
		admin := true
		_, err = st.UpdateUser(u.ID, &hash, &admin)
		return err == nil, generated, err
	}
	_, err = st.CreateUser(name, hash, true)
	return err == nil, generated, err
}

// ResetPassword sets a new password for the named user, creating them as an
// administrator if they do not exist. All their sessions end. An empty password
// is replaced by a random one, returned in generated.
func ResetPassword(st *store.Store, name, password string) (generated string, err error) {
	if !auth.ValidUsername(name) {
		return "", errors.New("invalid user name")
	}
	if password == "" {
		password = auth.RandomToken(12)
		generated = password
	}
	if err := auth.ValidPassword(password); err != nil {
		return "", err
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return "", err
	}
	u, err := st.GetUserByName(name)
	if errors.Is(err, store.ErrNotFound) {
		_, err = st.CreateUser(name, hash, true)
		return generated, err
	}
	if err != nil {
		return "", err
	}
	_, err = st.UpdateUser(u.ID, &hash, nil)
	return generated, err
}
