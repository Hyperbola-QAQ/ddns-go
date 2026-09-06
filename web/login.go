package web

import (
	"crypto/rand"
	"embed"
	"encoding/json"
	"fmt"
	"github.com/jeessy2/ddns-go/v6/audit"
	"html/template"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/jeessy2/ddns-go/v6/config"
	"github.com/jeessy2/ddns-go/v6/util"
)

//go:embed login.html
var loginEmbedFile embed.FS

// CookieName cookie name
const cookieName = "token"

// CookieInSystem only one cookie
var sessionMu sync.RWMutex

var sessions = map[string]http.Cookie{}

// 服务启动时间
var startTime = time.Now()

// 保存限制时间
const saveLimit = time.Duration(30) * time.Minute

// 登录失败锁定时间
const loginFailLockDuration = time.Duration(30) * time.Minute

// 登录检测
type loginDetect struct {
	mu          sync.Mutex
	failedTimes uint32       // 失败次数
	ticker      *time.Ticker // 定时器
}

var ld = &loginDetect{ticker: time.NewTicker(5 * time.Minute)}

// Login login page
func Login(writer http.ResponseWriter, request *http.Request) {
	tmpl, err := template.ParseFS(loginEmbedFile, "login.html")
	if err != nil {
		fmt.Println("Error happened..")
		fmt.Println(err)
		return
	}

	conf, _ := config.GetConfigCached()

	err = tmpl.Execute(writer, struct {
		EmptyUser bool   // 未填写用户名和密码
		Lang      string // 当前后端语言
	}{
		EmptyUser: conf.Username == "" && conf.Password == "",
		Lang:      conf.Lang,
	})
	if err != nil {
		fmt.Println("Error happened..")
		fmt.Println(err)
	}
}

// LoginFunc login func
func LoginFunc(w http.ResponseWriter, r *http.Request) {
	config.EditMu.Lock()
	defer config.EditMu.Unlock()
	ld.mu.Lock()
	defer ld.mu.Unlock()
	conf, _ := config.GetConfigCached()
	util.InitLogLang(conf.Lang)

	if ld.failedTimes >= 5 {
		loginUnlock()
		returnError(w, util.LogStr("登录失败次数过多，请稍后再试"))
		return
	}

	// 从请求中读取 JSON 数据
	var data struct {
		Username string `json:"Username"`
		Password string `json:"Password"`
	}

	err := json.NewDecoder(r.Body).Decode(&data)

	if err != nil {
		returnError(w, err.Error())
		return
	}

	// 用户名密码不能为空
	if data.Username == "" || data.Password == "" {
		returnError(w, util.LogStr("必须输入用户名/密码"))
		return
	}

	// 初始化用户名密码
	if conf.Username == "" && conf.Password == "" {
		if time.Since(startTime) > saveLimit {
			returnError(w, util.LogStr("需在 %s 之前完成用户名密码设置,请重启ddns-go", startTime.Add(saveLimit).Format("2006-01-02 15:04:05")))
			return
		}

		conf.NotAllowWanAccess = true
		u, err := url.Parse(r.Header.Get("referer"))
		if err == nil && !util.IsPrivateNetwork(u.Host) {
			conf.NotAllowWanAccess = false
		}

		conf.Username = data.Username
		hashedPwd, err := conf.CheckPassword(data.Password)
		if err != nil {
			returnError(w, err.Error())
			return
		}
		conf.Password = hashedPwd
		conf.ClusterDefaults()
		bumpClusterVersion(&conf)
		if err := conf.SaveConfig(); err != nil {
			returnError(w, "保存账户失败")
			return
		}
		queueClusterSync()
		recordChange(r, conf, "account.create", "", conf.Cluster.LocalID, nil, conf.Username)
	}

	// 登录
	if data.Username == conf.Username && util.PasswordOK(conf.Password, data.Password) {
		conf.ClusterDefaults()
		ld.ticker.Stop()
		ld.failedTimes = 0

		// 设置cookie过期时间为1天
		timeoutDays := 1
		if conf.NotAllowWanAccess {
			// 内网访问cookie过期时间为30天
			timeoutDays = 30
		}

		sessionMu.Lock()
		pruneSessions(time.Now())
		cookie := http.Cookie{Name: cookieName, Value: rand.Text(), Path: "/", Expires: time.Now().AddDate(0, 0, timeoutDays), HttpOnly: true, SameSite: http.SameSiteLaxMode}
		sessions[cookie.Value] = cookie
		http.SetCookie(w, &cookie)
		token := cookie.Value
		sessionMu.Unlock()
		audit.Record(audit.Event{Actor: conf.Username, Host: util.GetRequestIPStr(r), Operation: "login", Node: conf.Cluster.LocalID, Result: "ok"})

		util.Log("%q 登录成功", util.GetRequestIPStr(r))

		returnOK(w, util.LogStr("登录成功"), token)
		return
	}

	audit.Record(audit.Event{Actor: data.Username, Host: util.GetRequestIPStr(r), Operation: "login", Node: conf.Cluster.LocalID, Result: "failed"})
	ld.failedTimes = ld.failedTimes + 1
	util.Log("%q 帐号密码不正确", util.GetRequestIPStr(r))
	returnError(w, util.LogStr("用户名或密码错误"))
}

// loginUnlock login unlock, reset failed login attempts
func loginUnlock() {
	ld.failedTimes = ld.failedTimes + 1
	ld.ticker.Reset(loginFailLockDuration)

	go func(ticker *time.Ticker) {
		for range ticker.C {
			ld.mu.Lock()
			ld.failedTimes = 4
			ticker.Stop()
			ld.mu.Unlock()
		}
	}(ld.ticker)

}

func invalidateSessions() {
	sessionMu.Lock()
	sessions = map[string]http.Cookie{}
	sessionMu.Unlock()
}

// Called under sessionMu; sessions expire independently of peer availability.
func pruneSessions(now time.Time) {
	for token, cookie := range sessions {
		if !cookie.Expires.After(now) {
			delete(sessions, token)
		}
	}
}
