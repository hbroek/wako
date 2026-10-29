package server

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hbroek/wako/internal/auth"
	"github.com/hbroek/wako/internal/store"
)

type userJSON struct {
	Name    string `json:"name"`
	IsAdmin bool   `json:"is_admin"`
	Created string `json:"created,omitempty"`
}

func userView(u *store.User) userJSON {
	return userJSON{Name: u.Name, IsAdmin: u.IsAdmin, Created: u.CreatedAt.UTC().Format(time.RFC3339)}
}

// accountAPI handles the sign-in, account, user and member endpoints. It reports
// whether the request was one of them.
func (s *Server) accountAPI(w http.ResponseWriter, r *http.Request, parts []string) bool {
	if s.auth == nil {
		switch parts[0] {
		case "login", "logout", "refresh", "me", "tokens", "users":
			http.Error(w, "sign-in is disabled on this server", http.StatusNotFound)
			return true
		}
	}
	switch {
	case len(parts) == 1 && parts[0] == "login":
		s.login(w, r)
	case len(parts) == 1 && parts[0] == "logout":
		s.logout(w, r)
	case len(parts) == 1 && parts[0] == "refresh":
		// The middleware refreshes expired access tokens by itself; this lets a
		// client do it explicitly. It answers with who is signed in.
		s.me(w, r)
	case len(parts) == 1 && parts[0] == "me":
		s.me(w, r)
	case len(parts) >= 1 && parts[0] == "tokens":
		s.tokens(w, r, parts[1:])
	case len(parts) >= 1 && parts[0] == "users":
		s.users(w, r, parts[1:])
	case len(parts) >= 4 && parts[0] == "spaces" && parts[2] == "members":
		s.members(w, r, parts[1], parts[3:])
	case len(parts) == 3 && parts[0] == "spaces" && parts[2] == "members":
		s.members(w, r, parts[1], nil)
	default:
		return false
	}
	return true
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a := s.auth
	var in struct{ Name, Password string }
	if !readJSON(w, r, &in) {
		return
	}
	addr := s.clientAddr(r)
	key := "u:" + strings.ToLower(in.Name) + "|" + addr
	for _, k := range []struct {
		l   *auth.Limiter
		key string
	}{{a.perAddr, "a:" + addr}, {a.perUser, key}} {
		if blocked, wait := k.l.Blocked(k.key); blocked {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait/time.Second)+1))
			jsonError(w, http.StatusTooManyRequests, "too many failed attempts; try again later")
			return
		}
	}
	if _, ok := s.cookieSecure(r); !ok {
		jsonError(w, http.StatusBadRequest, "refusing to sign in over plain HTTP: use HTTPS, or start the server with --allow-insecure")
		return
	}
	u, err := s.st.GetUserByName(in.Name)
	if err != nil {
		auth.BurnPassword(in.Password) // same work whether or not the name exists
	}
	if err != nil || !auth.VerifyPassword(in.Password, u.PasswordHash) {
		a.perAddr.Fail("a:" + addr)
		a.perUser.Fail(key)
		jsonError(w, http.StatusUnauthorized, "invalid name or password")
		return
	}
	a.perUser.Reset(key)
	s.startSession(w, r, u)
	writeJSON(w, http.StatusOK, userView(u))
}

// startSession signs u in on this client with fresh cookies.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u *store.User) {
	now := s.auth.now()
	refresh := auth.RandomToken(32)
	sid, err := s.st.CreateSession(u.ID, auth.HashToken(refresh), now, now.Add(s.auth.refreshTTL))
	if err != nil {
		s.fail(w, err)
		return
	}
	s.setSessionCookies(w, r, u.ID, sid, refresh)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if p := s.principal(r); p != nil && p.cookie {
		s.st.DeleteSession(p.sid)
	}
	s.clearCookies(w, r)
	w.WriteHeader(http.StatusNoContent)
}

type meJSON struct {
	userJSON
	Spaces map[string]string `json:"spaces"` // slug -> role, for spaces the user is a member of
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	p := s.principal(r)
	if p == nil {
		s.deny(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		out := meJSON{userJSON: userView(p.user), Spaces: map[string]string{}}
		roles, _ := s.st.UserRoles(p.user.ID)
		if spaces, err := s.st.ListSpaces(); err == nil {
			for _, sp := range spaces {
				if role, ok := roles[sp.ID]; ok {
					out.Spaces[sp.Slug] = string(role)
				}
			}
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPut: // change own password
		if !p.cookie {
			http.Error(w, "change the password while signed in with a browser session", http.StatusForbidden)
			return
		}
		var in struct {
			Current string `json:"current_password"`
			New     string `json:"new_password"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		if !auth.VerifyPassword(in.Current, p.user.PasswordHash) {
			jsonError(w, http.StatusForbidden, "the current password is wrong")
			return
		}
		if err := auth.ValidPassword(in.New); err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		hash, err := auth.HashPassword(in.New)
		if err != nil {
			s.fail(w, err)
			return
		}
		if _, err := s.st.UpdateUser(p.user.ID, &hash, nil); err != nil { // ends every session
			s.fail(w, err)
			return
		}
		s.startSession(w, r, p.user) // keep this browser signed in
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, PUT")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ---- API tokens ----

func (s *Server) tokens(w http.ResponseWriter, r *http.Request, rest []string) {
	p := s.principal(r)
	if p == nil {
		s.deny(w, r)
		return
	}
	if !p.cookie { // tokens are managed from a browser session, never with a token
		http.Error(w, "manage API tokens while signed in with a browser session", http.StatusForbidden)
		return
	}
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		list, err := s.st.ListTokens(p.user.ID)
		if err != nil {
			s.fail(w, err)
			return
		}
		out := []map[string]any{}
		for _, t := range list {
			out = append(out, map[string]any{"id": t.ID, "name": t.Name, "created": t.CreatedAt.UTC().Format(time.RFC3339), "suggest_only": t.SuggestOnly})
		}
		writeJSON(w, http.StatusOK, out)
	case len(rest) == 0 && r.Method == http.MethodPost:
		var in struct {
			Name        string
			SuggestOnly bool `json:"suggest_only"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		name := strings.TrimSpace(in.Name)
		if name == "" || len(name) > 80 {
			jsonError(w, http.StatusBadRequest, "give the token a name of up to 80 characters")
			return
		}
		secret := "wako_" + auth.RandomToken(32)
		t, err := s.st.CreateTokenWith(p.user.ID, name, auth.HashToken(secret), in.SuggestOnly)
		if err != nil {
			s.fail(w, err)
			return
		}
		// The secret is shown once; only its hash is kept.
		writeJSON(w, http.StatusCreated, map[string]any{"id": t.ID, "name": t.Name, "token": secret, "suggest_only": t.SuggestOnly})
	case len(rest) == 1 && r.Method == http.MethodDelete:
		id, err := strconv.ParseInt(rest[0], 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		if err := s.st.DeleteToken(p.user.ID, id); err != nil {
			s.fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ---- users (server administrators) ----

func (s *Server) users(w http.ResponseWriter, r *http.Request, rest []string) {
	if s.principal(r) == nil {
		s.deny(w, r)
		return
	}
	if !s.isAdmin(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		list, err := s.st.ListUsers()
		if err != nil {
			s.fail(w, err)
			return
		}
		out := []userJSON{}
		for _, u := range list {
			out = append(out, userView(u))
		}
		writeJSON(w, http.StatusOK, out)
	case len(rest) == 0 && r.Method == http.MethodPost:
		var in struct {
			Name     string `json:"name"`
			Password string `json:"password"`
			IsAdmin  bool   `json:"is_admin"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		if !auth.ValidUsername(in.Name) {
			jsonError(w, http.StatusBadRequest, "a name uses letters, digits, '.', '-' and '_' (up to 64) and starts with a letter or digit")
			return
		}
		if err := auth.ValidPassword(in.Password); err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		hash, err := auth.HashPassword(in.Password)
		if err != nil {
			s.fail(w, err)
			return
		}
		u, err := s.st.CreateUser(in.Name, hash, in.IsAdmin)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, userView(u))
	case len(rest) == 1 && r.Method == http.MethodPut:
		u, err := s.st.GetUserByName(rest[0])
		if err != nil {
			s.fail(w, err)
			return
		}
		var in struct {
			Password *string `json:"password"`
			IsAdmin  *bool   `json:"is_admin"`
		}
		if !readJSON(w, r, &in) {
			return
		}
		var hash *string
		if in.Password != nil {
			if err := auth.ValidPassword(*in.Password); err != nil {
				jsonError(w, http.StatusBadRequest, err.Error())
				return
			}
			h, err := auth.HashPassword(*in.Password)
			if err != nil {
				s.fail(w, err)
				return
			}
			hash = &h
		}
		u, err = s.st.UpdateUser(u.ID, hash, in.IsAdmin)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, userView(u))
	case len(rest) == 1 && r.Method == http.MethodDelete:
		u, err := s.st.GetUserByName(rest[0])
		if err != nil {
			s.fail(w, err)
			return
		}
		if err := s.st.DeleteUser(u.ID); err != nil {
			s.fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, POST, PUT, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// ---- space members (space administrators) ----

type memberJSON struct {
	Name string `json:"name"`
	Role string `json:"role"`
}

func (s *Server) members(w http.ResponseWriter, r *http.Request, slug string, rest []string) {
	sp, err := s.st.GetSpace(slug)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	acc := s.access(r, sp)
	if !acc.Read {
		http.NotFound(w, r)
		return
	}
	if !acc.Admin {
		s.deny(w, r)
		return
	}
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		list, err := s.st.ListMembers(sp.ID)
		if err != nil {
			s.fail(w, err)
			return
		}
		out := []memberJSON{}
		for _, m := range list {
			out = append(out, memberJSON{m.User.Name, string(m.Role)})
		}
		writeJSON(w, http.StatusOK, out)
	case len(rest) == 1 && r.Method == http.MethodPut:
		var in struct{ Role string }
		if !readJSON(w, r, &in) {
			return
		}
		role := store.Role(in.Role)
		if !store.ValidRole(role) {
			jsonError(w, http.StatusBadRequest, "role must be viewer, suggester, editor or admin")
			return
		}
		u, err := s.st.GetUserByName(rest[0])
		if err != nil {
			jsonError(w, http.StatusNotFound, "there is no user with that name")
			return
		}
		if err := s.st.SetMember(sp.ID, u.ID, role); err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, memberJSON{u.Name, in.Role})
	case len(rest) == 1 && r.Method == http.MethodDelete:
		u, err := s.st.GetUserByName(rest[0])
		if err != nil {
			s.fail(w, err)
			return
		}
		if err := s.st.RemoveMember(sp.ID, u.ID); err != nil {
			s.fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
