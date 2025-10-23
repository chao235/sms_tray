package main

import (
	_ "embed"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/getlantern/systray"
	"github.com/gin-gonic/gin"
	"github.com/go-toast/toast"
	"golang.design/x/clipboard"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

//go:embed assets/icon.ico
var iconData []byte

var (
	serverRunning bool
	server        *gin.Engine
	serverThread  *httpServerWrapper
	logFile       *os.File
	logLock       sync.Mutex
)

// ---------- 包装 gin.Server 用于控制启动停止 ----------
type httpServerWrapper struct {
	addr   string
	server *gin.Engine
	stopCh chan struct{}
}

func (s *httpServerWrapper) Start() {
	go func() {
		writeLog("监听端口", s.addr)
		err := s.server.Run(s.addr)
		if err != nil {
			writeLog("HTTP 启动错误:", err)
		}
	}()
}

func (s *httpServerWrapper) Stop() {
	// Gin 没有直接 Close，需要 http.Server 实例时才能优雅关闭。
	// 这里可以简单退出 goroutine。
	close(s.stopCh)
	writeLog("HTTP 服务关闭")
}

// go build -ldflags="-H=windowsgui" -o sms_service.exe
// ---------- 主函数 ----------
func main() {
	setupLog()
	systray.Run(onReady, onExit)
}

// ---------- 日志 ----------
func setupLog() {
	var err error
	logFile, err = os.OpenFile("sms-service.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		fmt.Println("创建日志文件失败:", err)
		os.Exit(1)
	}
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.SetOutput(io.MultiWriter(logFile))
	writeLog("程序启动")
}

func writeLog(v ...any) {
	logLock.Lock()
	defer logLock.Unlock()
	log.Println(v...)
	logFile.Sync()
}

// ---------- 托盘 ----------
func onReady() {
	systray.SetIcon(iconData)
	systray.SetTitle("短信服务")
	systray.SetTooltip("短信验证码托盘服务")

	mStart := systray.AddMenuItem("启动服务", "启动 HTTP 服务")
	mStop := systray.AddMenuItem("停止服务", "停止 HTTP 服务")
	mLog := systray.AddMenuItem("打开日志", "查看日志文件")
	mAuto := systray.AddMenuItemCheckbox("开机自启", "开机时自动运行", isAutoRun())
	mQuit := systray.AddMenuItem("退出", "退出程序")

	mStop.Disable()

	// ✅ 启动托盘时自动启动一次服务
	go func() {
		startServer()
		serverRunning = true
		mStart.Disable()
		mStop.Enable()
		writeLog("HTTP 服务已自动启动")
	}()

	go func() {
		for {
			select {
			case <-mStart.ClickedCh:
				if !serverRunning {
					startServer()
					serverRunning = true
					mStart.Disable()
					mStop.Enable()
					writeLog("HTTP 服务已启动")
				}
			case <-mStop.ClickedCh:
				if serverRunning {
					stopServer()
					serverRunning = false
					mStop.Disable()
					mStart.Enable()
					writeLog("HTTP 服务已停止")
				}
			case <-mAuto.ClickedCh:
				if mAuto.Checked() {
					disableAutoRun()
					mAuto.Uncheck()
					writeLog("已关闭开机自启")
				} else {
					exePath, _ := os.Executable()
					enableAutoRun("sms-service", exePath)
					mAuto.Check()
					writeLog("已启用开机自启")
				}
			case <-mLog.ClickedCh:
				exec.Command("notepad.exe", "sms-service.log").Start()
			case <-mQuit.ClickedCh:
				onExit()
				systray.Quit()
				return
			}
		}
	}()
}

// ---------- Gin 服务 ----------
func startServer() {
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	r.POST("/copy", func(c *gin.Context) {
		content := c.PostForm("content")
		writeLog("收到 /copy 消息:", content)
		showToast("手机短信", content)

		reCode := regexp.MustCompile(`验证码[\s\S]*?(\d+)`)
		matches := reCode.FindStringSubmatch(content)
		code := ""
		if len(matches) > 1 {
			code = matches[1]
			writeLog("提取验证码:", code)
		} else {
			showToast("手机短信", "未找到验证码")
			writeLog("未找到验证码")
		}

		if err := clipboard.Init(); err == nil {
			clipboard.Write(clipboard.FmtText, []byte(code))
			pasteClipboard()
		}
		c.String(200, "success")
	})

	r.POST("/msg", func(c *gin.Context) {
		content := c.PostForm("content")
		if content != "" {
			writeLog("收到 /msg 消息:", content)
			showToast("手机消息", content)
		}
		c.String(200, "success")
	})

	serverThread = &httpServerWrapper{
		addr:   ":9002",
		server: r,
		stopCh: make(chan struct{}),
	}

	serverThread.Start()
}

func stopServer() {
	if serverThread != nil {
		serverThread.Stop()
	}
}

// ---------- Toast ----------
func showToast(title, msg string) {
	iconPath, _ := extractIcon()
	notification := toast.Notification{
		AppID:   "短信服务",
		Title:   title,
		Message: msg,
		Icon:    iconPath,
	}
	notification.Push()
}

func extractIcon() (string, error) {
	tmpDir := os.TempDir()
	iconPath := filepath.Join(tmpDir, "tray_icon.ico")
	err := os.WriteFile(iconPath, iconData, 0644)
	if err != nil {
		return "", err
	}
	return iconPath, nil
}

// ---------- 模拟粘贴 ----------
func pasteClipboard() {
	const KEYEVENTF_KEYUP = 0x0002
	kbd := windows.NewLazySystemDLL("user32.dll").NewProc("keybd_event")
	ctrl := byte(0x11)
	v := byte(0x56)
	kbd.Call(uintptr(ctrl), 0, 0, 0)
	time.Sleep(100 * time.Millisecond)
	kbd.Call(uintptr(v), 0, 0, 0)
	time.Sleep(100 * time.Millisecond)
	kbd.Call(uintptr(v), 0, KEYEVENTF_KEYUP, 0)
	kbd.Call(uintptr(ctrl), 0, KEYEVENTF_KEYUP, 0)
	writeLog("已执行 Ctrl+V 粘贴操作")
}

// ---------- 退出 ----------
func onExit() {
	stopServer()
	writeLog("程序退出")
	logFile.Sync()
	logFile.Close()
}

// ---------- 开机自启 ----------
func enableAutoRun(name, path string) {
	k, _, err := registry.CreateKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Run`,
		registry.ALL_ACCESS)
	if err != nil {
		writeLog("注册表写入失败:", err)
		return
	}
	defer k.Close()
	err = k.SetStringValue(name, "\""+path+"\"")
	if err != nil {
		writeLog("设置开机启动项失败:", err)
	}
}

func disableAutoRun() {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Run`,
		registry.ALL_ACCESS)
	if err == nil {
		defer k.Close()
		k.DeleteValue("sms-service")
	}
}

func isAutoRun() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Run`,
		registry.READ)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue("sms-service")
	return err == nil
}
