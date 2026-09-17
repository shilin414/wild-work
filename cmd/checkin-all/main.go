// Command checkin-all 独立签到工具：遍历 auths 目录下所有账号逐个签到，
// 单个账号失败只记录并继续下一个，最后打印汇总与失败清单。
//
// 与常驻 daemon 里的签到有什么区别：
//  1. 不看 state.json 的 disabled / cooling 状态 —— 停用、冷却中的账号照常签到；
//  2. 不写 state.json —— 签到不会把账号解冻、不会塞回路由池；
//  3. 默认只写 auths 下的凭证文件（token 过期时刷新后原子写回）。
//
// 用法（把 exe 放在 config.json 同级目录，或用 -config 指定）：
//
//	checkin-all                  # 签到所有 workbuddy + traework 账号
//	checkin-all -dry-run         # 只列出将要签到的账号与 token 剩余有效期，不发请求
//	checkin-all -force-refresh   # 顺带强制保活：无条件刷新所有账号 token
//	checkin-all -only workbuddy
//	checkin-all -uid 123,456
//	checkin-all -json            # 机器可读输出
//
// token 有效期说明（重要）：
//   · 签到接口本身不刷新任何有效期，它只领积分；
//   · 默认只在 token 剩余不足 2 小时时才顺带 refresh，所以日常签到几乎不会续期；
//   · 真正负责续期的是常驻进程的 keepalive（默认 22:00），而它跳过停用账号，
//     所以长期停用的账号 token 会一路衰减到期 —— 需要时用 -force-refresh 手动保活；
//   · -force-refresh 会轮换 refresh token 并写回文件，与常驻 wild-work 并存时
//     可能让进程内存里的旧 refresh token 作废，建议先退出常驻进程再执行。
//
// 退出码：0 = 全部成功；1 = 有失败账号；2 = 参数/加载错误。
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/config"
	"wild-work/internal/provider"
	"wild-work/internal/traework"
	"wild-work/internal/upstream"
)

// result 单个账号的签到结果。
type result struct {
	Platform  string `json:"platform"`
	UID       string `json:"uid"`
	Name      string `json:"name,omitempty"`
	OK        bool   `json:"ok"`
	Msg       string `json:"msg"`
	Remain    int64  `json:"remain,omitempty"`
	HasRemain bool   `json:"has_remain,omitempty"`
	TokenLeft string `json:"token_left,omitempty"`
}

// refreshMode token 刷新策略。
type refreshMode int

const (
	refreshOff   refreshMode = iota // 完全不刷新
	refreshAuto                     // 仅在剩余不足 2 小时时刷新
	refreshForce                    // 无条件刷新（手动保活）
)

type job struct {
	platform string
	up       provider.Upstream
	a        *auth.Auth
}

func main() {
	log.SetFlags(0)

	var (
		cfgPath   = flag.String("config", "config.json", "配置文件路径（相对路径先取当前目录，再取 exe 同目录）")
		authDir   = flag.String("auth-dir", "", "账号目录，默认取配置里的 auth_dir")
		only      = flag.String("only", "", "只签某个渠道：workbuddy / traework（留空 = 全部）")
		uids      = flag.String("uid", "", "只签指定 uid，逗号分隔（留空 = 全部）")
		timeout   = flag.Int("timeout", 0, "HTTP 超时秒数，0 = 用配置里的 upstream.timeout_seconds")
		noRefresh = flag.Bool("no-refresh", false, "不刷新 token，直接用现有 access token 签到")
		forceRef  = flag.Bool("force-refresh", false, "无条件刷新所有账号 token（保活）；refresh token 会被轮换，建议先退出常驻 wild-work")
		strict    = flag.Bool("strict", false, "余额查询失败也计为签到失败")
		asJSON    = flag.Bool("json", false, "以 JSON 输出结果")
		dryRun    = flag.Bool("dry-run", false, "只列出将要签到的账号，不发起任何网络请求")
	)
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "加载配置失败：%v\n", err)
		os.Exit(2)
	}

	sec := cfg.Upstream.TimeoutSeconds
	if *timeout > 0 {
		sec = *timeout
	}
	if sec <= 0 {
		sec = 120
	}
	httpTimeout := time.Duration(sec) * time.Second

	dir := *authDir
	if dir == "" {
		dir = cfg.AuthDir
	}

	*only = strings.ToLower(strings.TrimSpace(*only))
	if *only != "" && *only != "workbuddy" && *only != "traework" && *only != "qoder" {
		fmt.Fprintf(os.Stderr, "-only 只支持 workbuddy / traework / qoder\n")
		os.Exit(2)
	}
	want := parseUIDs(*uids)

	wbUp := upstream.New()
	wbUp.HTTP.Timeout = httpTimeout
	trUp := traework.New()
	trUp.HTTP.Timeout = httpTimeout

	jobs := make([]job, 0)
	if *only == "" || *only == "workbuddy" {
		list, err := auth.LoadWorkBuddyDir(dir, cfg.Region)
		if err != nil {
			fmt.Fprintf(os.Stderr, "读取 WorkBuddy 账号目录失败：%v\n", err)
			os.Exit(2)
		}
		for _, a := range list {
			jobs = append(jobs, job{platform: "workbuddy", up: wbUp, a: a})
		}
	}
	if *only == "" || *only == "traework" {
		list, err := auth.LoadTraeDir(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "读取 TraeWork 账号目录失败：%v\n", err)
			os.Exit(2)
		}
		for _, a := range list {
			jobs = append(jobs, job{platform: "traework", up: trUp, a: a})
		}
	}
	if *only == "qoder" {
		fmt.Println("qoder 渠道上游无签到活动，无需签到。")
		return
	}

	if want != nil {
		filtered := make([]job, 0, len(jobs))
		for _, j := range jobs {
			if want[j.a.UID] {
				filtered = append(filtered, j)
			}
		}
		jobs = filtered
	}
	sort.Slice(jobs, func(i, k int) bool {
		if jobs[i].platform != jobs[k].platform {
			return jobs[i].platform < jobs[k].platform
		}
		return displayName(jobs[i].a) < displayName(jobs[k].a)
	})

	if len(jobs) == 0 {
		fmt.Println("没有匹配的账号（检查 -auth-dir / -only / -uid）")
		return
	}

	if *dryRun {
		fmt.Printf("dry-run：将签到以下 %d 个账号（不发起请求）\n", len(jobs))
		for _, j := range jobs {
			fmt.Printf("  [%-9s] %-24s uid=%-36s token剩余 %s\n", j.platform, displayName(j.a), j.a.UID, expiryLeft(j.a))
		}
		fmt.Println("\nqoder 渠道无签到活动，始终跳过。")
		return
	}

	results := make([]result, 0, len(jobs))
	for _, j := range jobs {
		results = append(results, runOne(j, refreshModeOf(*noRefresh, *forceRef), *strict))
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(results)
	} else {
		printReport(results)
	}

	failed := 0
	for _, r := range results {
		if !r.OK {
			failed++
		}
	}
	if failed > 0 {
		os.Exit(1)
	}
}

// runOne 对单个账号签到；任何错误都被捕获进结果，不影响后续账号。
func runOne(j job, mode refreshMode, strict bool) result {
	r := result{Platform: j.platform, UID: j.a.UID, Name: j.a.Nickname}
	if j.a.RefreshToken == "" {
		r.Msg = "no refresh token（凭证文件缺少 refresh token）"
		return r
	}

	var note string
	refreshed := false
	switch mode {
	case refreshForce:
		if err := j.up.RefreshToken(j.a); err != nil {
			note = "token 刷新失败，已用旧 token 尝试"
		} else if err := j.a.SaveAtomic(); err != nil {
			note = "token 已刷新但写回文件失败"
		}
		refreshed = true
	case refreshAuto:
		if j.a.NeedsRefresh(2 * time.Hour) {
			if err := j.up.RefreshToken(j.a); err != nil {
				// 刷新失败不一定意味着签到失败：access token 可能仍在有效期内。
				// 记录提示后继续尝试，最终结果以上游返回为准。
				note = "token 刷新失败，已用旧 token 尝试"
			} else if err := j.a.SaveAtomic(); err != nil {
				note = "token 已刷新但写回文件失败"
			}
			refreshed = true
		}
	}

	err := j.up.DailyCheckin(j.a)
	if err != nil && isSessionDead(err) && mode != refreshOff && !refreshed {
		if rerr := j.up.RefreshToken(j.a); rerr == nil {
			_ = j.a.SaveAtomic()
			err = j.up.DailyCheckin(j.a)
		}
	}

	switch {
	case err == nil:
		r.OK = true
		r.Msg = joinMsg(note, "ok")
	case isAlready(err):
		r.OK = true
		r.Msg = joinMsg(note, "今日已签到")
	default:
		r.Msg = joinMsg(note, shortErr(err))
	}

	// 余额只用于展示：查不到不改变签到成败，除非开了 -strict。
	remain, rerr := j.up.UserResource(j.a)
	if rerr != nil {
		if strict {
			r.OK = false
			r.Msg = joinMsg(r.Msg, "余额查询失败："+shortErr(rerr))
		}
	} else {
		r.Remain, r.HasRemain = remain, true
	}
	r.TokenLeft = expiryLeft(j.a)
	return r
}

// refreshModeOf 把两个互斥开关折叠成刷新策略。
func refreshModeOf(noRefresh, force bool) refreshMode {
	if noRefresh {
		return refreshOff
	}
	if force {
		return refreshForce
	}
	return refreshAuto
}

// expiryLeft 返回 access token 的剩余有效期（人类可读）。
func expiryLeft(a *auth.Auth) string {
	if a.ExpiresAt <= 0 {
		return "未知"
	}
	d := time.Until(time.Unix(a.ExpiresAt, 0))
	if d <= 0 {
		return "已过期"
	}
	if d < 48*time.Hour {
		return fmt.Sprintf("%.1f 小时", d.Hours())
	}
	return fmt.Sprintf("%.1f 天", d.Hours()/24)
}

func printReport(results []result) {
	fmt.Println("=== 签到明细 ===")
	for _, r := range results {
		status := "失败"
		if r.OK {
			status = "成功"
		}
		line := fmt.Sprintf("  [%-9s] %-24s %s", r.Platform, displayNameOf(r), status)
		if r.HasRemain {
			line += fmt.Sprintf("  剩余 %d", r.Remain)
		}
		if r.TokenLeft != "" {
			line += "  token " + r.TokenLeft
		}
		line += "  " + r.Msg
		fmt.Println(line)
	}

	ok, failed := 0, make([]result, 0)
	for _, r := range results {
		if r.OK {
			ok++
		} else {
			failed = append(failed, r)
		}
	}
	fmt.Printf("\n=== 汇总 ===\n总计 %d，成功 %d，失败 %d\n", len(results), ok, len(failed))

	if len(failed) == 0 {
		fmt.Println("全部签到成功。")
		return
	}
	fmt.Println("\n=== 失败清单 ===")
	for _, r := range failed {
		fmt.Printf("  [%-9s] %-24s uid=%s\n            原因：%s\n", r.Platform, displayNameOf(r), r.UID, r.Msg)
	}
}

// ---------------------------------------------------------------------------
// 辅助
// ---------------------------------------------------------------------------

func loadConfig(path string) (*config.Config, error) {
	if path == "" {
		return config.Default(), nil
	}
	cfg, err := config.Load(path)
	if err == nil {
		return cfg, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	// 相对路径：退回 exe 所在目录再找一次（双击 exe 时 cwd 可能不是 bin 目录）。
	if !filepath.IsAbs(path) {
		if exe, e := os.Executable(); e == nil {
			alt := filepath.Join(filepath.Dir(exe), path)
			cfg2, err2 := config.Load(alt)
			if err2 == nil {
				return cfg2, nil
			}
			if !errors.Is(err2, fs.ErrNotExist) {
				return nil, err2
			}
		}
	}
	// 没有配置文件时按默认参数继续（auth 目录默认 ./auths）。
	return config.Default(), nil
}

func parseUIDs(s string) map[string]bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	out := map[string]bool{}
	for _, part := range strings.Split(s, ",") {
		if uid := strings.TrimSpace(part); uid != "" {
			out[uid] = true
		}
	}
	return out
}

func displayName(a *auth.Auth) string {
	if a.Nickname != "" {
		return a.Nickname
	}
	if a.UID == "" {
		return "(unknown)"
	}
	if len(a.UID) > 10 {
		return a.UID[:10]
	}
	return a.UID
}

func displayNameOf(r result) string {
	if r.Name != "" {
		return r.Name
	}
	if len(r.UID) > 10 {
		return r.UID[:10]
	}
	return r.UID
}

// isAlready 只匹配明确的“今日已签到”，不因错误文本里含 checkin 就判成功。
func isAlready(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "已签到") ||
		strings.Contains(s, "already check") ||
		strings.Contains(s, "already checked") ||
		strings.Contains(s, "code=9095") ||
		strings.Contains(s, "code=14001")
}

func isSessionDead(err error) bool {
	var ue *provider.Error
	return errors.As(err, &ue) && ue.Kind == provider.ErrSessionDead
}

func shortErr(err error) string {
	s := strings.TrimSpace(err.Error())
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

func joinMsg(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return strings.Join(kept, "；")
}
