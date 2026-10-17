package phonelogin

import (
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"testing"
)

// Keycloak 下发的会话 cookie 作用域是 Path=/auth/realms/copilot/（HAR #114 原文）。
// Go 的 cookiejar 按路径过滤，用根路径查不到 —— 这条测试钉住修复，否则手机号登录
// 会在"最后一跳明明成功"的情况下永远报「未能建立登录会话」。
func TestHasKeycloakSessionRespectsRealmPath(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("jar: %v", err)
	}
	host := "https://www.workbuddy.cn"
	scope, err := url.Parse(host + "/auth/realms/copilot/")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	jar.SetCookies(scope, []*http.Cookie{
		{Name: "KEYCLOAK_SESSION", Value: "copilot/uid/sid", Path: "/auth/realms/copilot/"},
	})
	s := &Session{client: &http.Client{Jar: jar}}
	if !s.hasKeycloakSession(host) {
		t.Fatal("realm 路径下的 KEYCLOAK_SESSION 应判为已建立会话")
	}
}

// 没有 cookie 时必须返回 false（防止把"永远 true"写进去，那种修复比 bug 更坏）。
func TestHasKeycloakSessionWithoutCookie(t *testing.T) {
	jar, _ := cookiejar.New(nil)
	s := &Session{client: &http.Client{Jar: jar}}
	if s.hasKeycloakSession("https://www.workbuddy.cn") {
		t.Fatal("没有 cookie 时应返回 false")
	}
}

// 客户端/Jar 为空时不能 panic。
func TestHasKeycloakSessionNilClient(t *testing.T) {
	if (&Session{}).hasKeycloakSession("https://www.workbuddy.cn") {
		t.Fatal("nil client 应返回 false")
	}
}
