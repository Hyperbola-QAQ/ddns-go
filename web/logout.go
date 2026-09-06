package web

import (
	"github.com/jeessy2/ddns-go/v6/config"
	"net/http"
	"time"
)

func Logout(w http.ResponseWriter, r *http.Request) {
	sessionMu.Lock()
	if cookie, err := r.Cookie(cookieName); err == nil {
		delete(sessions, cookie.Value)
	}
	pruneSessions(time.Now())
	sessionMu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", Expires: time.Unix(0, 0), MaxAge: -1, HttpOnly: true})
	c, _ := config.GetConfigCached()
	recordChange(r, c, "logout", "", c.Cluster.LocalID, "logged in", "logged out")

	// 重定向用户到登录页面
	http.Redirect(w, r, "./login", http.StatusFound)
}
