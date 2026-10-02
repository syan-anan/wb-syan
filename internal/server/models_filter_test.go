package server

import (
	"reflect"
	"testing"

	"github.com/syan-anan/wb-syan/internal/keys"
)

// /v1/models 名单过滤：受限子密钥只看到白名单内的模型，而不是"列全部、只放开一条通道"。
func TestFilterModelsForIdentity(t *testing.T) {
	list := []map[string]any{
		{"id": "cn:glm-5.3"},
		{"id": "cn:hy3-turbo"},
		{"id": "cn:gpt-5.1"},
		{"id": "global:claude-sonnet-4.5"},
		{"id": "global:gpt-5.1"},
	}
	ids := func(in []map[string]any) []string {
		out := make([]string, 0, len(in))
		for _, e := range in {
			out = append(out, e["id"].(string))
		}
		return out
	}

	// 主 key：原样返回（同一底层切片，零拷贝零回归）。
	if got := filterModelsForIdentity(list, keys.Identity{Master: true}); ids(got) == nil || len(got) != len(list) {
		t.Fatalf("主 key 必须原样返回，got %v", ids(got))
	}
	// 未设任何白名单：原样返回。
	if got := filterModelsForIdentity(list, keys.Identity{KeyID: "k"}); len(got) != len(list) {
		t.Fatalf("无白名单必须原样返回，got %v", ids(got))
	}

	cases := []struct {
		name string
		id   keys.Identity
		want []string
	}{
		{
			name: "模型白名单只留命中的一条",
			id:   keys.Identity{KeyID: "k1", Models: []string{"glm-5.3"}},
			want: []string{"cn:glm-5.3"},
		},
		{
			name: "模型白名单前缀通配 + 大小写不敏感",
			id:   keys.Identity{KeyID: "k2", Models: []string{"HY3-*"}},
			want: []string{"cn:hy3-turbo"},
		},
		{
			name: "模型白名单跨域命中（裸名判定）",
			id:   keys.Identity{KeyID: "k3", Models: []string{"gpt-5.1"}},
			want: []string{"cn:gpt-5.1", "global:gpt-5.1"},
		},
		{
			name: "域白名单 global 只留国际版",
			id:   keys.Identity{KeyID: "k4", Realms: []string{"global"}},
			want: []string{"global:claude-sonnet-4.5", "global:gpt-5.1"},
		},
		{
			name: "模型 + 域双白名单取交集",
			id:   keys.Identity{KeyID: "k5", Models: []string{"gpt-5.1"}, Realms: []string{"global"}},
			want: []string{"global:gpt-5.1"},
		},
		{
			name: "全不命中返回空列表",
			id:   keys.Identity{KeyID: "k6", Models: []string{"nonexistent"}},
			want: []string{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := filterModelsForIdentity(list, c.id)
			if got == nil {
				t.Fatal("必须返回非 nil 切片（JSON 需序列化为 []）")
			}
			if !reflect.DeepEqual(ids(got), c.want) {
				t.Fatalf("got %v want %v", ids(got), c.want)
			}
		})
	}

	// 入参不得被修改（modelList 结果被面板等其它调用方复用）。
	if len(list) != 5 || list[0]["id"] != "cn:glm-5.3" {
		t.Fatalf("入参被就地修改：%v", ids(list))
	}
}
