// Package app wails 绑定层：向托盘面板前端暴露账号/签到/积分/配置操作，
// 内部驱动 pool / scheduler / upstream / HTTP 服务。
package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/rockswang/workbuddy-wild/internal/auth"
	"github.com/rockswang/workbuddy-wild/internal/config"
	"github.com/rockswang/workbuddy-wild/internal/login"
	logintrae "github.com/rockswang/workbuddy-wild/internal/login_trae"
	"github.com/rockswang/workbuddy-wild/internal/pool"
	"github.com/rockswang/workbuddy-wild/internal/provider"
	"github.com/rockswang/workbuddy-wild/internal/scheduler"
	"github.com/rockswang/workbuddy-wild/internal/server"
	"github.com/rockswang/workbuddy-wild/internal/winutil"
)

// Version 面板展示的版本号。
const Version = "0.3.0"

const (
	loginTimeout   = 5 * time.Minute
	loginPollEvery = 2 * time.Second
)

// Options 构建 App 的依赖。
type Runtime struct {
	Kind      provider.Kind
	Pool      *pool.Pool
	Upstream  provider.Upstream
	Scheduler *scheduler.Scheduler
}

type Options struct {
	ConfigPath string
	Config     *config.Config
	Runtimes   map[provider.Kind]*Runtime
	Handler    *server.Handler
}

// App 托盘面板后端。
type App struct {
	cfgPath  string
	cfg      *config.Config
	runtimes map[provider.Kind]*Runtime
	handler  *server.Handler

	mu      sync.Mutex // 保护 httpSrv / cfg 修改
	httpSrv *http.Server

	ctx context.Context // wails runtime ctx（OnStartup 后可用）

	muLogin      sync.Mutex
	loginBusy    bool
	loginCtx     context.Context
	loginCancel  context.CancelFunc
	loginClient  *http.Client
	loginStateFP string
	loginKind    provider.Kind

	domReadyCh chan struct{} // 前端就绪信号（ShowPanel 等待，避免白窗口）

	logFile *os.File
}

// New 构建 App 并接管全局日志（写文件 + 环形缓冲 + 事件推送）。
func New(opts Options) (*App, error) {
	a := &App{
		cfgPath:    opts.ConfigPath,
		cfg:        opts.Config,
		runtimes:   opts.Runtimes,
		handler:    opts.Handler,
		domReadyCh: make(chan struct{}),
	}
	a.loginStateFP = filepath.Join(filepath.Dir(opts.Config.StateFile), "login-state.json")

	// 日志文件 data/app.log（与 state.json 同目录）
	logFP := filepath.Join(filepath.Dir(opts.Config.StateFile), "app.log")
	if err := os.MkdirAll(filepath.Dir(logFP), 0o755); err == nil {
		f, err := os.OpenFile(logFP, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err == nil {
			a.logFile = f
		}
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetOutput(&logWriter{app: a})
	return a, nil
}

// Close 关闭日志文件。
func (a *App) Close() {
	if a.logFile != nil {
		_ = a.logFile.Close()
		a.logFile = nil
	}
}

func (a *App) runtime(kind provider.Kind) *Runtime {
	if a.runtimes == nil {
		return nil
	}
	return a.runtimes[kind]
}

func (a *App) firstRuntime() *Runtime {
	for _, k := range []provider.Kind{provider.WorkBuddy, provider.TraeWork} {
		if rt := a.runtime(k); rt != nil {
			return rt
		}
	}
	return nil
}

func (a *App) totalAccounts() int {
	n := 0
	for _, rt := range a.runtimes {
		if rt != nil && rt.Pool != nil {
			n += len(rt.Pool.List())
		}
	}
	return n
}

func (a *App) allStatuses() []pool.Status {
	out := []pool.Status{}
	for _, rt := range a.runtimes {
		if rt != nil && rt.Pool != nil {
			out = append(out, rt.Pool.List()...)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UID < out[j].UID })
	return out
}

func (a *App) findRuntimeAuth(uid string) (*Runtime, *auth.Auth) {
	for _, rt := range a.runtimes {
		if rt == nil || rt.Pool == nil {
			continue
		}
		if au := rt.Pool.AuthByUID(uid); au != nil {
			return rt, au
		}
	}
	return nil, nil
}

func (a *App) checkinHours() []int {
	if rt := a.firstRuntime(); rt != nil && rt.Scheduler != nil {
		return rt.Scheduler.CheckinHours()
	}
	return nil
}

func (a *App) checkinTimes() []string {
	if rt := a.firstRuntime(); rt != nil && rt.Scheduler != nil {
		return rt.Scheduler.CheckinTimes()
	}
	return nil
}

func (a *App) keepaliveHours() []int {
	if rt := a.firstRuntime(); rt != nil && rt.Scheduler != nil {
		return rt.Scheduler.KeepaliveHours()
	}
	return nil
}

func (a *App) nextFire() time.Time {
	if rt := a.firstRuntime(); rt != nil && rt.Scheduler != nil {
		return rt.Scheduler.NextFire()
	}
	return time.Time{}
}

// ---------------------------------------------------------------------------
// 生命周期
// ---------------------------------------------------------------------------

// StartServer 按当前配置启动 HTTP 服务（监听失败返回错误）。
func (a *App) StartServer() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.serveLocked(a.cfg.Listen.Addr())
}

// serveLocked 在 newAddr 上启动新服务并切换；调用方需持有 a.mu。
func (a *App) serveLocked(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           a.handler,
		ReadHeaderTimeout: 30 * time.Second,
	}
	old := a.httpSrv
	a.httpSrv = srv
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("http serve %s: %v", addr, err)
		}
	}()
	if old != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = old.Shutdown(shutdownCtx)
	}
	return nil
}

// OnStartup wails 窗口就绪回调：定位面板、隐藏任务栏按钮、推送初始状态。
func (a *App) OnStartup(ctx context.Context) {
	a.ctx = ctx
	a.positionPanel()
	winutil.HideFromTaskbar(winutil.MainWindow())
	log.Printf("workbuddy-wild %s 已启动，API 监听 %s（账号 %d）",
		Version, a.cfg.Listen.Addr(), a.totalAccounts())
	a.emitAccounts()
}

// OnDomReady 前端就绪回调：关闭 domReadyCh，通知 ShowPanel 可以弹窗。
func (a *App) OnDomReady(ctx context.Context) {
	select {
	case <-a.domReadyCh:
	default:
		close(a.domReadyCh)
	}
}

// ShowStartupNotice 弹出“已启动 + API 地址”提示框。
// 由 main 在 StartServer 后立即调用（早于托盘/窗口初始化），体验即时。
func (a *App) ShowStartupNotice() {
	a.safeGo(a.showStartupNotice)
}

// ShowSecondInstanceNotice 已运行实例被再次双击：询问打开面板或退出
// （面板异常时仍可从这里退出，保证“退得出去”）。
func (a *App) ShowSecondInstanceNotice() {
	a.safeGo(func() {
		host := a.cfg.Listen.Host
		if host == "" || host == "0.0.0.0" || host == "::" {
			host = "127.0.0.1"
		}
		if winutil.AskYesNo("WorkBuddy-Wild 已启动",
			fmt.Sprintf("OpenAI 兼容 API 地址：\nhttp://%s:%d\n\n是否打开管理面板？\n（选择“否”将退出程序）", host, a.cfg.Listen.Port)) {
			a.ShowPanel()
		} else {
			a.Quit()
		}
	})
}

// showStartupNotice 显示“仅可关闭”的启动提示框。
func (a *App) showStartupNotice() {
	host := a.cfg.Listen.Host
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1" // 面板/提示里展示本机可达地址
	}
	winutil.InfoBox("WorkBuddy-Wild 已启动",
		fmt.Sprintf("OpenAI 兼容 API 地址：\nhttp://%s:%d\n\n点击右下角托盘图标打开管理面板。", host, a.cfg.Listen.Port))
}

// OnShutdown wails 退出回调：关闭 HTTP 服务与日志。
func (a *App) OnShutdown(ctx context.Context) {
	a.mu.Lock()
	srv := a.httpSrv
	a.httpSrv = nil
	a.mu.Unlock()
	if srv != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}
	a.Close()
}

// panelGap 面板与屏幕边缘（及任务栏）的间隙。
const panelGap = 12

// panelRect 计算面板最终位置（贴主任务栏/工作区右下角）。
// 高度随账号数量自适应：0 账号基础高度 500，每 +1 账号增高 55（账号卡片实测约 55px），
// 最多 4 个封顶（列表内部滚动）。退出按钮用 margin-top:auto 贴底，无需精确高度。
//
// 返回 (x, y) 为面板左上角<物理像素>坐标（喂给 WindowSetPosition，其原样加到物理工作区左上角），
// (pw, ph) 为 DIP 尺寸（喂给 WindowSetSize，其内部按 DPI 放大回物理）。二者单位不同，
// 若用同一批坐标喂给两个 API，在缩放显示屏上窗口物理尺寸会被放大而位置不变，导致右下角越界。
func (a *App) panelRect() (x, y, pw, ph int) {
	sc, logW, logH := a.dpiScale()
	if sc <= 0 || logW <= 0 || logH <= 0 {
		sc = 1 // 拿不到 DPI 时退化为 1:1，仍做屏幕内钳制
	}
	scrW, scrH := winutil.ScreenSize() // 原生物理像素
	waX, waY, waW, waH := winutil.WorkArea()
	tbX, tbY, tbW, tbH, ok := winutil.TaskbarRect()
	if !ok {
		tbX, tbY, tbW, tbH = 0, 0, 0, 0
	}
	pw, ph = 270, 500+minInt(a.totalAccounts(), 4)*55
	x, y, pw, ph = winutil.AnchorPanel(int(waX), int(waY), int(waW), int(waH),
		int(tbX), int(tbY), int(tbW), int(tbH), sc, int(scrW), int(scrH), pw, ph, panelGap)
	return x, y, pw, ph
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// dpiScale 返回 (DPI 缩放系数 sc, 主屏逻辑宽, 主屏逻辑高)。
// sc = 物理像素 ÷ DIP = Screen.Width ÷ Screen.Size.Width：
// wails 的 Screen.Width/Height 来自 EnumDisplayMonitors 的 RECT，是<物理像素>；
// Screen.Size 经 ScaleToDefaultDPI 换算，才是逻辑 DIP。取二者比值恰好得到 DPI 系数
// （125% 缩放下 sc=1.25），供 AnchorPanel 统一把 DIP 尺寸换算到物理坐标。
func (a *App) dpiScale() (float64, int, int) {
	if a.ctx == nil {
		return 0, 0, 0
	}
	screens, err := runtime.ScreenGetAll(a.ctx)
	if err != nil || len(screens) == 0 {
		return 0, 0, 0
	}
	var s runtime.Screen
	found := false
	for _, sc := range screens {
		if sc.IsPrimary {
			s = sc
			found = true
			break
		}
	}
	if !found && len(screens) > 0 {
		s = screens[0]
	}
	if s.Width <= 0 || s.Size.Width <= 0 {
		return 0, 0, 0
	}
	sc := float64(s.Width) / float64(s.Size.Width)
	return sc, s.Size.Width, s.Size.Height
}

// positionPanel 把面板定位到右下角（贴任务栏），尺寸按账号数自适应。
func (a *App) positionPanel() {
	a.resizePanel()
}

var showMu sync.Mutex // 串行化 ShowPanel（防动画/定位竞态）

// ShowPanel 弹出面板。WebView2 崩溃或未就绪时通知用户并继续运行。
func (a *App) ShowPanel() {
	host := a.cfg.Listen.Host
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	addr := fmt.Sprintf("http://%s:%d", host, a.cfg.Listen.Port)
	// WebView2 从未初始化（早期 wails.Run 失败，以无头模式运行）
	if a.ctx == nil {
		winutil.InfoBox("面板不可用",
			"管理面板未初始化。\n\n"+
				"HTTP 服务正常运行中，可通过 API 地址直接调用：\n"+addr)
		return
	}
	// 检测 WebView2 进程是否已崩溃（窗口句柄为 0）
	if hwnd := winutil.MainWindow(); hwnd == 0 {
		winutil.InfoBox("面板已崩溃",
			"管理面板（WebView2）已意外退出。\n"+
				"HTTP 服务与自动签到仍在正常运行。\n\n"+
				"如需恢复面板，请重启程序。\n\n"+
				"API 地址："+addr)
		return
	}
	select {
	case <-a.domReadyCh:
		a.showPanelNow()
	default:
		// 冷启动首屏：等前端就绪再弹；90s 兜底防死等
		go func() {
			select {
			case <-a.domReadyCh:
			case <-time.After(90 * time.Second):
				a.showPanelNow()
			}
		}()
	}
}

// showPanelNow 实际显示面板（右下角上滑动效）并异步刷新数据。
// 注意：托盘回调里必须以 go a.ShowPanel() 调用，避免阻塞托盘消息循环。
func (a *App) showPanelNow() {
	if a.ctx == nil {
		return
	}
	showMu.Lock()
	defer showMu.Unlock()
	fx, fy, pw, ph := a.panelRect()
	// 起始位置：低 48px，再上滑到位（“从托盘弹出”效果）
	runtime.WindowSetSize(a.ctx, pw, ph)
	runtime.WindowSetPosition(a.ctx, fx, fy+48)
	runtime.WindowShow(a.ctx)
	runtime.EventsEmit(a.ctx, "panel:shown", nil) // 前端据此重置失焦宽限期 + 触发内容动效
	winutil.FocusWindow(winutil.MainWindow())
	// 上滑动画：8 步 × 6px，约 150ms；结束再补一次激活规避前台锁
	a.safeGo(func() {
		for i := 1; i <= 8; i++ {
			time.Sleep(19 * time.Millisecond)
			runtime.WindowSetPosition(a.ctx, fx, fy+48-i*6)
		}
		winutil.FocusWindow(winutil.MainWindow())
	})
	a.safeGo(a.RefreshAll)
}

// HidePanel 隐藏面板（点击收起按钮 / Esc）。单进程下窗口隐藏，托盘点击再显示。
func (a *App) HidePanel() {
	if a.ctx != nil {
		runtime.WindowHide(a.ctx)
	}
}

// Quit 退出应用（触发 wails 关闭流程）。
func (a *App) Quit() {
	if a.ctx != nil {
		runtime.Quit(a.ctx)
	}
}

// QuitAll 退出整个程序（前端“退出”按钮调用）。单进程模式下 = Quit。
func (a *App) QuitAll() {
	a.Quit()
}

// ---------------------------------------------------------------------------
// 面板数据
// ---------------------------------------------------------------------------

// AccountView 面板展示的账号（脱敏）。
type AccountView struct {
	UID            string `json:"uid"`
	Group          string `json:"group"` // workbuddy | traework（平台图标区分）
	Nickname       string `json:"nickname"`
	Credits        int64  `json:"credits"`
	Cooling        bool   `json:"cooling"`
	Until          string `json:"until"`
	Reason         string `json:"reason"`
	Disabled       bool   `json:"disabled"`
	ErrCount       int    `json:"err_count"`
	LastCheckinOK  bool   `json:"last_checkin_ok"`
	LastCheckinAt  string `json:"last_checkin_at"`
	LastCheckinMsg string `json:"last_checkin_msg"`
}

// State 面板初始数据。
type State struct {
	Accounts       []AccountView `json:"accounts"`
	CheckinHours   []int         `json:"checkin_hours"` // 旧前端兼容
	CheckinTimes   []string      `json:"checkin_times"`
	KeepaliveHours []int         `json:"keepalive_hours"`
	ListenHost     string        `json:"listen_host"`
	ListenPort     int           `json:"listen_port"`
	APIKey         string        `json:"api_key"`
	LoginBusy      bool          `json:"login_busy"`
	NextCheckin    string        `json:"next_checkin"`
	Version        string        `json:"version"`
	Autostart      bool          `json:"autostart"`
	Running        bool          `json:"running"`
}

// GetState 返回面板初始数据。
func (a *App) GetState() State {
	st := State{
		CheckinHours:   a.checkinHours(),
		CheckinTimes:   a.checkinTimes(),
		KeepaliveHours: a.keepaliveHours(),
		ListenHost:     a.cfg.Listen.Host,
		ListenPort:     a.cfg.Listen.Port,
		APIKey:         a.cfg.APIKey,
		LoginBusy:      a.loginActive(),
		NextCheckin:    fmtTime(a.nextFire()),
		Version:        Version,
		Autostart:      winutil.AutostartEnabled(),
		Running:        a.serverRunning(),
	}
	st.Accounts = a.accountViews()
	return st
}

// accountViews 账号列表（按 UID 排序）。
func (a *App) accountViews() []AccountView {
	statuses := a.allStatuses()
	out := make([]AccountView, 0, len(statuses))
	for _, s := range statuses {
		out = append(out, AccountView{
			UID:            s.UID,
			Group:          a.accountGroup(s.UID),
			Nickname:       s.Nickname,
			Credits:        s.Credits,
			Cooling:        s.Cooling,
			Until:          fmtTime(s.Until),
			Reason:         s.Reason,
			Disabled:       s.Disabled,
			ErrCount:       s.ErrCount,
			LastCheckinOK:  s.LastCheckinOK,
			LastCheckinAt:  fmtTime(s.LastCheckinAt),
			LastCheckinMsg: s.LastCheckinMsg,
		})
	}
	return out
}

// accountGroup 返回账号所属分组（workbuddy/traework）。
// 依据 auth 文件路径前缀：trae-*.json → traework，workbuddy-*.json → workbuddy。
func (a *App) accountGroup(uid string) string {
	_, au := a.findRuntimeAuth(uid)
	if au != nil {
		if au.Kind != "" {
			return au.Kind
		}
		if au.FilePath != "" && strings.HasPrefix(filepath.Base(au.FilePath), "trae-") {
			return "traework"
		}
	}
	return "workbuddy"
}

func (a *App) serverRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.httpSrv != nil
}

func (a *App) loginActive() bool {
	a.muLogin.Lock()
	defer a.muLogin.Unlock()
	return a.loginBusy
}

// ---------------------------------------------------------------------------
// 账号操作
// ---------------------------------------------------------------------------

// StartLogin 发起 WorkBuddy 登录（兼容旧前端绑定）。
func (a *App) StartLogin() (string, error) { return a.StartLoginFor(provider.WorkBuddy.String()) }

// StartLoginFor 发起指定平台登录：workbuddy / traework。
func (a *App) StartLoginFor(kind string) (string, error) {
	k := provider.Kind(strings.TrimSpace(kind))
	if k == "" {
		k = provider.WorkBuddy
	}
	if k != provider.WorkBuddy && k != provider.TraeWork {
		return "", fmt.Errorf("unknown login provider %s", kind)
	}
	a.muLogin.Lock()
	if a.loginBusy {
		a.muLogin.Unlock()
		return "", errors.New("已有登录流程进行中，请先完成或取消")
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.loginCtx, a.loginCancel = ctx, cancel
	a.loginKind = k
	if k == provider.TraeWork {
		a.loginClient = logintrae.NewClient()
	} else {
		a.loginClient = login.NewClient()
	}
	a.loginBusy = true
	a.muLogin.Unlock()

	var authURL string
	var err error
	if k == provider.TraeWork {
		authURL, err = logintrae.Start(a.loginClient, a.loginStateFP)
	} else {
		authURL, err = login.Start(a.loginClient, a.loginStateFP)
		if err == nil {
			// 手动跟随登录页跳转链（copilot.tencent.com → codebuddy.cn），浏览器直接打开最终地址
			if resolved, rerr := login.ResolveAuthURL(a.loginClient, authURL); rerr == nil && resolved != "" {
				authURL = resolved
			}
		}
	}
	if err != nil {
		a.finishLogin()
		return "", err
	}
	go a.launchBrowser(authURL)
	a.safeGo(func() { a.pollLogin(ctx) })
	log.Printf("%s 登录流程已发起", k)
	return authURL, nil
}

// CancelLogin 取消当前登录流程。
func (a *App) CancelLogin() error {
	a.muLogin.Lock()
	cancel := a.loginCancel
	ctx := a.loginCtx
	a.muLogin.Unlock()
	if cancel == nil {
		return errors.New("没有进行中的登录")
	}
	cancel()
	_ = ctx
	_ = os.Remove(a.loginStateFP)
	log.Printf("登录已取消")
	return nil
}

// launchBrowser 无痕拉起默认浏览器；失败则用系统默认方式兜底。
func (a *App) launchBrowser(authURL string) {
	if exe, flag, ok := winutil.DefaultBrowserIncognito(); ok {
		if err := winutil.LaunchIncognito(exe, flag, authURL); err == nil {
			return
		}
	}
	_ = winutil.OpenURL(authURL)
}

// pollLogin 后台轮询登录结果，事件推送到前端。
func (a *App) pollLogin(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("login poll panic: %v", r)
		}
		a.finishLogin()
	}()
	deadline := time.Now().Add(loginTimeout)
	a.emitLogin("waiting", "已打开浏览器，请在无痕窗口中完成登录…")
	t := time.NewTicker(loginPollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			a.emitLogin("cancelled", "登录已取消")
			return
		case <-t.C:
		}
		if time.Now().After(deadline) {
			a.emitLogin("failed", "登录超时，请重新发起")
			return
		}
		if a.loginKind == provider.TraeWork {
			r, err := logintrae.Poll(a.loginClient, a.loginStateFP)
			if err == nil {
				a.completeTraeLogin(r)
				return
			}
			if errors.Is(err, logintrae.ErrPending) {
				a.emitLogin("waiting", "等待 Trae 浏览器回调…")
			} else {
				log.Printf("trae login poll failed: %v", err)
				a.emitLogin("waiting", "Trae 登录轮询失败，自动重试："+shortErr(err))
			}
			continue
		}
		r, err := login.Poll(a.loginClient, a.loginStateFP)
		if err == nil {
			a.completeLogin(r)
			return
		}
		if errors.Is(err, login.ErrPending) {
			a.emitLogin("waiting", "等待浏览器完成登录…")
		} else {
			log.Printf("workbuddy login poll failed: %v", err)
			a.emitLogin("waiting", "登录轮询失败，自动重试："+shortErr(err))
		}
	}
}

// completeLogin 登录成功：写 auth 文件、重载账号池，然后异步签到 + 查积分
// （慢网络下签到可能耗时较长，不能阻塞登录完成的提示）。
func (a *App) completeLogin(r login.Result) {
	log.Printf("workbuddy 登录成功 uid=%s nickname=%s expires_in=%d refresh_token=%t", r.UID, r.Nickname, r.ExpiresIn, r.RefreshToken != "")
	fp, err := login.SaveAuth(a.cfg.AuthDir, r)
	if err != nil {
		log.Printf("workbuddy 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		a.emitLogin("failed", "保存凭证失败: "+err.Error())
		return
	}
	log.Printf("workbuddy 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	name := r.Nickname
	if name == "" && len(r.UID) >= 8 {
		name = r.UID[:8]
	}
	a.emitLogin("success", fmt.Sprintf("登录成功：%s（正在同步积分…）", name))
	a.finishLogin() // 立即释放登录锁，允许再次发起
	a.safeGo(func() {
		rt := a.runtime(provider.WorkBuddy)
		if rt == nil || rt.Scheduler == nil {
			return
		}
		res, err := rt.Scheduler.CheckinAccount(r.UID)
		if err != nil {
			log.Printf("新账号签到失败 %s: %v", name, err)
		} else {
			remain := "-"
			if res.HasRemain {
				remain = fmt.Sprintf("%d", res.Remain)
			}
			log.Printf("新账号签到完成 %s：%s，剩余积分 %s", name, res.Msg, remain)
		}
		a.emitAccounts()
	})
}

func (a *App) completeTraeLogin(r logintrae.Result) {
	log.Printf("traework 登录成功 uid=%s nickname=%s expires_at=%d refresh_token=%t", r.UID, r.Nickname, r.ExpiresAt, r.RefreshToken != "")
	fp, err := logintrae.SaveAuth(a.cfg.AuthDir, r)
	if err != nil {
		log.Printf("traework 登录保存凭证失败 uid=%s err=%v", r.UID, err)
		a.emitLogin("failed", "保存凭证失败: "+err.Error())
		return
	}
	log.Printf("traework 登录凭证已保存 uid=%s file=%s", r.UID, filepath.Base(fp))
	a.reloadAccounts()
	name := r.Nickname
	if name == "" && len(r.UID) >= 8 {
		name = r.UID[:8]
	}
	a.emitLogin("success", fmt.Sprintf("TraeWork 登录成功：%s（正在同步积分…）", name))
	a.finishLogin()
	a.safeGo(func() {
		rt := a.runtime(provider.TraeWork)
		if rt == nil || rt.Scheduler == nil {
			return
		}
		res, err := rt.Scheduler.CheckinAccount(r.UID)
		if err != nil {
			log.Printf("TraeWork 新账号签到失败 %s: %v", name, err)
		} else {
			log.Printf("TraeWork 新账号签到完成 %s：%s", name, res.Msg)
		}
		a.emitAccounts()
	})
}

func (a *App) finishLogin() {
	a.muLogin.Lock()
	a.loginBusy = false
	a.loginCtx, a.loginCancel, a.loginClient = nil, nil, nil
	a.loginKind = ""
	a.muLogin.Unlock()
}

// reloadAccounts 用 auths 目录最新文件对齐账号池。
func (a *App) reloadAccounts() {
	if rt := a.runtime(provider.WorkBuddy); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadWorkBuddyDir(a.cfg.AuthDir, a.cfg.Region)
		if err != nil {
			log.Printf("reload workbuddy accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	if rt := a.runtime(provider.TraeWork); rt != nil && rt.Pool != nil {
		auths, err := auth.LoadTraeDir(a.cfg.AuthDir)
		if err != nil {
			log.Printf("reload traework accounts: %v", err)
		} else {
			rt.Pool.SyncToDir(auths)
		}
	}
	a.emitAccounts()
}

// CheckinAccount 单个账号立即签到。
func (a *App) CheckinAccount(uid string) (scheduler.CheckinResult, error) {
	rt, _ := a.findRuntimeAuth(uid)
	if rt == nil || rt.Scheduler == nil {
		return scheduler.CheckinResult{}, fmt.Errorf("unknown account %s", uid)
	}
	res, err := rt.Scheduler.CheckinAccount(uid)
	a.emitAccounts()
	if err != nil {
		log.Printf("checkin failed uid=%s err=%v", uid, err)
		return res, err
	}
	log.Printf("checkin uid=%s ok=%t msg=%s remain=%d has_remain=%t", uid, res.OK, res.Msg, res.Remain, res.HasRemain)
	return res, nil
}

// CheckinAll 全部账号立即签到，返回每个账号的真实结果（包括失败）。
func (a *App) CheckinAll() []scheduler.CheckinResult {
	results := make([]scheduler.CheckinResult, 0)
	for _, rt := range a.runtimes {
		if rt == nil || rt.Pool == nil || rt.Scheduler == nil {
			continue
		}
		for _, st := range rt.Pool.List() {
			if st.Disabled {
				res := scheduler.CheckinResult{UID: st.UID, Msg: "账号已禁用"}
				results = append(results, res)
				log.Printf("checkin result platform=%s uid=%s ok=false msg=%s", rt.Kind, st.UID, res.Msg)
				continue
			}
			res, err := rt.Scheduler.CheckinAccount(st.UID)
			if err != nil {
				res = scheduler.CheckinResult{UID: st.UID, Msg: err.Error()}
				log.Printf("checkin result platform=%s uid=%s ok=false msg=%s", rt.Kind, st.UID, res.Msg)
			}
			results = append(results, res)
		}
	}
	a.emitAccounts()
	ok := 0
	for _, r := range results {
		if r.OK {
			ok++
		}
	}
	log.Printf("批量签到完成：total=%d ok=%d failed=%d", len(results), ok, len(results)-ok)
	return results
}

// RefreshCredits 刷新单个账号积分。
func (a *App) RefreshCredits(uid string) (int64, error) {
	rt, au := a.findRuntimeAuth(uid)
	if rt == nil || au == nil {
		return 0, fmt.Errorf("unknown account %s", uid)
	}
	log.Printf("credits refresh start platform=%s uid=%s", rt.Kind, uid)
	remain, err := rt.Upstream.UserResource(au)
	if err != nil {
		log.Printf("credits refresh failed platform=%s uid=%s err=%v", rt.Kind, uid, err)
		return 0, err
	}
	rt.Pool.SetCredits(uid, remain)
	log.Printf("credits refresh success platform=%s uid=%s remain=%d", rt.Kind, uid, remain)
	a.emitAccounts()
	return remain, nil
}

// RefreshAll 刷新全部账号积分（面板打开时调用）。
func (a *App) RefreshAll() {
	for _, rt := range a.runtimes {
		if rt == nil || rt.Pool == nil || rt.Upstream == nil {
			continue
		}
		for _, st := range rt.Pool.List() {
			au := rt.Pool.AuthByUID(st.UID)
			if au == nil || au.AccessToken == "" {
				continue
			}
			log.Printf("credits refresh start platform=%s uid=%s", rt.Kind, st.UID)
			if remain, err := rt.Upstream.UserResource(au); err == nil {
				rt.Pool.SetCredits(st.UID, remain)
				log.Printf("credits refresh success platform=%s uid=%s remain=%d", rt.Kind, st.UID, remain)
			} else {
				log.Printf("credits refresh failed platform=%s uid=%s err=%v", rt.Kind, st.UID, err)
			}
		}
	}
	a.emitAccounts()
}

// RemoveAccount 删除账号（auth 文件 + 内存池）。
func (a *App) RemoveAccount(uid string) error {
	rt, au := a.findRuntimeAuth(uid)
	if rt == nil || au == nil {
		return fmt.Errorf("unknown account %s", uid)
	}
	if au.FilePath != "" {
		_ = os.Remove(au.FilePath)
	}
	rt.Pool.Remove(uid)
	a.emitAccounts()
	log.Printf("已删除账号 %s", shortUID(uid))
	return nil
}

// ---------------------------------------------------------------------------
// 配置操作
// ---------------------------------------------------------------------------

// SetCheckinHours 保留旧前端 API，仅支持整点并转发到分钟配置。
func (a *App) SetCheckinHours(hours []int) error {
	minutes := make([]int, 0, len(hours))
	for _, h := range hours {
		minutes = append(minutes, h*60)
	}
	return a.SetCheckinMinutes(minutes)
}

// SetCheckinTimes 更新自动签到时间（HH:MM）并立即唤醒两个调度器。
func (a *App) SetCheckinTimes(times []string) error {
	minutes, err := config.ParseClockTimes(times)
	if err != nil {
		return err
	}
	return a.SetCheckinMinutes(minutes)
}

func (a *App) SetCheckinMinutes(minutes []int) error {
	clean := normalizeMinutes(minutes)
	if len(clean) == 0 {
		return errors.New("请至少保留一个签到时间")
	}
	times := make([]string, 0, len(clean))
	for _, m := range clean {
		times = append(times, fmt.Sprintf("%02d:%02d", m/60, m%60))
	}
	a.mu.Lock()
	a.cfg.Schedule.CheckinTimes = times
	a.cfg.Schedule.CheckinHours = nil // 避免旧整点配置造成歧义；旧读取器会使用默认行为
	err := config.Save(a.cfg, a.cfgPath)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	for _, rt := range a.runtimes {
		if rt != nil && rt.Scheduler != nil {
			rt.Scheduler.SetCheckinMinutes(clean)
		}
	}
	log.Printf("自动签到时间已更新：%s", strings.Join(times, "、"))
	return nil
}

// SetListen 修改 API 监听主机 + 端口：保存配置并热切换监听（失败保持原样）。
func (a *App) SetListen(host string, port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("端口无效：%d", port)
	}
	addr := config.Listen{Host: host, Port: port}.Addr()

	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.serveLocked(addr); err != nil {
		return fmt.Errorf("监听 %s 失败（可能被占用）：%v", addr, err)
	}
	a.cfg.Listen = config.Listen{Host: host, Port: port}
	if err := config.Save(a.cfg, a.cfgPath); err != nil {
		// 配置写回失败不回滚监听（服务已在跑），下次启动回退
		log.Printf("save config after listen change: %v", err)
	}
	log.Printf("API 监听已切换至 %s", addr)
	return nil
}

// SetAPIKey 修改 API 密钥：写回 config.json 并即时生效（handler 运行时更新）。
func (a *App) SetAPIKey(key string) error {
	key = strings.TrimSpace(key)
	a.mu.Lock()
	a.cfg.APIKey = key
	err := config.Save(a.cfg, a.cfgPath)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	a.handler.SetAPIKey(key)
	log.Printf("API-Key 已更新")
	return nil
}

// SetAutostart 设置/取消开机自启。
func (a *App) SetAutostart(on bool) error {
	if err := winutil.SetAutostart(on); err != nil {
		return err
	}
	if on {
		log.Printf("已开启开机自启")
	} else {
		log.Printf("已关闭开机自启")
	}
	return nil
}

// ---------------------------------------------------------------------------
// 事件与日志
// ---------------------------------------------------------------------------

// NotifyRefresh 将 token 刷新结果推送到日志和面板。
func (a *App) NotifyRefresh(platform, uid string, ok bool, msg string) {
	log.Printf("GUI refresh platform=%s uid=%s ok=%t msg=%s", platform, uid, ok, msg)
	if !ok && a.ctx != nil {
		runtime.EventsEmit(a.ctx, "refresh", map[string]any{"platform": platform, "uid": uid, "ok": ok, "msg": msg})
	}
	a.emitAccounts()
}

// NotifyCheckin 将自动/手动签到结果推送到日志和面板。
func (a *App) NotifyCheckin(platform string, r scheduler.CheckinResult) {
	log.Printf("GUI checkin platform=%s uid=%s ok=%t msg=%s remain=%d has_remain=%t", platform, r.UID, r.OK, r.Msg, r.Remain, r.HasRemain)
	a.emitAccounts()
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "checkin", map[string]any{
			"platform":   platform,
			"uid":        r.UID,
			"ok":         r.OK,
			"msg":        r.Msg,
			"remain":     r.Remain,
			"has_remain": r.HasRemain,
		})
	}
}

func (a *App) emitAccounts() {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "accounts", a.accountViews())
		// 账号数变化 → 面板高度自适应（重算尺寸并保持右下角位置）
		a.resizePanel()
	}
}

// resizePanel 按当前账号数重算面板尺寸并重定位（不打断前端交互）。
func (a *App) resizePanel() {
	if a.ctx == nil {
		return
	}
	fx, fy, pw, ph := a.panelRect()
	runtime.WindowSetSize(a.ctx, pw, ph)
	runtime.WindowSetPosition(a.ctx, fx, fy)
}

func (a *App) emitLogin(phase, msg string) {
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "login", map[string]string{"phase": phase, "msg": msg})
	}
}

// logWriter 写日志文件（GUI 无控制台，app.log 供“查看日志”使用）。
type logWriter struct{ app *App }

func (w *logWriter) Write(p []byte) (int, error) {
	if w.app.logFile != nil {
		_, _ = w.app.logFile.Write(p)
	}
	return len(p), nil
}

// OpenLogFile 用系统记事本打开日志文件（先确保文件存在）。
func (a *App) OpenLogFile() error {
	fp := filepath.Join(filepath.Dir(a.cfg.StateFile), "app.log")
	if _, err := os.Stat(fp); err != nil {
		_ = os.WriteFile(fp, []byte("(empty)\n"), 0o644)
	}
	abs, err := filepath.Abs(fp)
	if err != nil {
		return err
	}
	return winutil.OpenWithNotepad(abs)
}

// safeGo 带 panic 兜底的 goroutine 启动器（避免单个 goroutine 崩溃静默）。
func (a *App) safeGo(f func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("PANIC: %v", r)
			}
		}()
		f()
	}()
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

func normalizeHours(hours []int) []int {
	minutes := make([]int, 0, len(hours))
	for _, h := range hours {
		minutes = append(minutes, h*60)
	}
	out := normalizeMinutes(minutes)
	hoursOut := make([]int, 0, len(out))
	for _, m := range out {
		hoursOut = append(hoursOut, m/60)
	}
	return hoursOut
}

func normalizeMinutes(minutes []int) []int {
	seen := map[int]bool{}
	out := []int{}
	for _, m := range minutes {
		if m >= 0 && m < 24*60 && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	sort.Ints(out)
	return out
}

func hoursStr(hours []int) string {
	out := make([]string, 0, len(hours))
	for _, h := range hours {
		out = append(out, fmt.Sprintf("%02d:00", h))
	}
	return strings.Join(out, "、")
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("01-02 15:04")
}

func shortUID(uid string) string {
	if len(uid) <= 10 {
		return uid
	}
	return uid[:10] + "…"
}

func shortErr(err error) string {
	s := strings.TrimSpace(err.Error())
	if len(s) > 160 {
		return s[:160]
	}
	return s
}
