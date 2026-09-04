package main

import (
	"encoding/json"
	"fmt"
	"image/color"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// ============================================================
// DEVICE MODEL
// ============================================================

type Device struct {
	Class        string `json:"Class"`
	FriendlyName string `json:"FriendlyName"`
	InstanceID   string `json:"InstanceId"`
}

// ============================================================
// LANGUAGE
// ============================================================

type Language int

const (
	English Language = iota
	Vietnamese
)

func tr(lang Language, en, vi string) string {
	if lang == Vietnamese {
		return vi
	}

	return en
}

// ============================================================
// FORCED LIGHT / DARK THEME
// ============================================================

type forcedTheme struct {
	variant fyne.ThemeVariant
}

func (f *forcedTheme) Color(
	name fyne.ThemeColorName,
	_ fyne.ThemeVariant,
) color.Color {
	return theme.DefaultTheme().Color(name, f.variant)
}

func (f *forcedTheme) Font(style fyne.TextStyle) fyne.Resource {
	return theme.DefaultTheme().Font(style)
}

func (f *forcedTheme) Icon(name fyne.ThemeIconName) fyne.Resource {
	return theme.DefaultTheme().Icon(name)
}

func (f *forcedTheme) Size(name fyne.ThemeSizeName) float32 {
	return theme.DefaultTheme().Size(name)
}

// ============================================================
// POINTER / HAND CURSOR BUTTON
// ============================================================

// PointerButton behaves like a normal Fyne button.
// Enabled  -> Fyne/Windows hand pointer.
// Disabled -> Windows' native IDC_NO (Unavailable) cursor.
//
// We intentionally use the system cursor from user32.dll instead of
// drawing our own icon, so the cursor follows the user's Windows cursor
// scheme, DPI and accessibility settings.
type PointerButton struct {
	widget.Button
	disabled bool
}

var (
	user32DLL       = syscall.NewLazyDLL("user32.dll")
	procLoadCursorW = user32DLL.NewProc("LoadCursorW")
	procSetCursor   = user32DLL.NewProc("SetCursor")
)

// Win32 predefined cursor resource ID.
// IDC_NO = unavailable / prohibited cursor from the current Windows scheme.
const windowsIDCNo = uintptr(32648)

func setWindowsUnavailableCursor() {
	hCursor, _, _ := procLoadCursorW.Call(0, windowsIDCNo)
	if hCursor != 0 {
		procSetCursor.Call(hCursor)
	}
}

func NewPointerButton(text string, icon fyne.Resource, tapped func()) *PointerButton {
	b := &PointerButton{}
	b.Text = text
	b.Icon = icon
	b.OnTapped = tapped
	b.ExtendBaseWidget(b)
	return b
}

func NewPointerTextButton(text string, tapped func()) *PointerButton {
	return NewPointerButton(text, nil, tapped)
}

func (b *PointerButton) Disable() {
	b.disabled = true
	b.Button.Disable()
}

func (b *PointerButton) Enable() {
	b.disabled = false
	b.Button.Enable()
}

// Fyne has no standard "not allowed" cursor constant in v2.8.
// For disabled buttons we let Fyne use the normal cursor, then override it
// with the native Windows IDC_NO cursor from MouseIn/MouseMoved below.
func (b *PointerButton) Cursor() desktop.Cursor {
	if b.disabled {
		return desktop.DefaultCursor
	}
	return desktop.PointerCursor
}

func (b *PointerButton) MouseIn(ev *desktop.MouseEvent) {
	b.Button.MouseIn(ev)
	if b.disabled {
		setWindowsUnavailableCursor()
	}
}

func (b *PointerButton) MouseMoved(ev *desktop.MouseEvent) {
	b.Button.MouseMoved(ev)
	if b.disabled {
		setWindowsUnavailableCursor()
	}
}

func (b *PointerButton) MouseOut() {
	b.Button.MouseOut()
}

// ============================================================
// UI STATE
// ============================================================

type UI struct {
	app    fyne.App
	window fyne.Window

	lang Language

	initialDevices []Device
	initialMap     map[string]Device
	personal       []Device

	pendingBox  *fyne.Container
	personalBox *fyne.Container

	titleLabel       *widget.Label
	subtitleLabel    *widget.Label
	statusLabel      *widget.Label
	instructionLabel *widget.Label

	pendingTitle  *widget.Label
	personalTitle *widget.Label

	resultTitle *widget.Label
	resultLabel *widget.Label

	warningLabel *widget.Label

	removeButton  *PointerButton
	restartButton *PointerButton
	guideButton   *PointerButton

	languageSelect *widget.Select
	themeSelect    *widget.Select

	languageText *widget.Label
	themeText    *widget.Label

	scanning bool
	removed  bool

	mu sync.Mutex
}

// ============================================================
// POWERSHELL
// ============================================================

func runPowerShell(script string) ([]byte, error) {
	cmd := exec.Command(
		"powershell.exe",
		"-NoProfile",
		"-NonInteractive",
		"-ExecutionPolicy", "Bypass",
		"-Command",
		"[Console]::OutputEncoding=[System.Text.Encoding]::UTF8; "+script,
	)

	return cmd.CombinedOutput()
}

// ============================================================
// DEVICE SCAN
// ============================================================

func getInputDevices() ([]Device, error) {
	script := `
$devices = Get-PnpDevice -PresentOnly |
    Where-Object {
        $_.Class -eq 'Keyboard' -or
        $_.Class -eq 'Mouse'
    } |
    Select-Object Class, FriendlyName, InstanceId

@($devices) | ConvertTo-Json -Compress
`

	out, err := runPowerShell(script)

	if err != nil {
		return nil, fmt.Errorf(
			"Get-PnpDevice failed: %v\n%s",
			err,
			string(out),
		)
	}

	raw := strings.TrimSpace(string(out))

	if raw == "" || raw == "null" {
		return []Device{}, nil
	}

	var devices []Device

	if strings.HasPrefix(raw, "[") {
		if err := json.Unmarshal([]byte(raw), &devices); err != nil {
			return nil, err
		}
	} else {
		var d Device

		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			return nil, err
		}

		devices = []Device{d}
	}

	return devices, nil
}

func deviceMap(devices []Device) map[string]Device {
	result := make(map[string]Device)

	for _, d := range devices {
		if d.InstanceID == "" {
			continue
		}

		result[strings.ToUpper(d.InstanceID)] = d
	}

	return result
}

func findNewDevices(
	old map[string]Device,
	current []Device,
) []Device {

	var result []Device

	for _, d := range current {
		id := strings.ToUpper(d.InstanceID)

		if _, exists := old[id]; !exists {
			result = append(result, d)
		}
	}

	return result
}

func hasClass(devices []Device, class string) bool {
	for _, d := range devices {
		if strings.EqualFold(d.Class, class) {
			return true
		}
	}

	return false
}

// ============================================================
// REMOVE DEVICE
// ============================================================

func removeDevice(instanceID string) (string, error) {
	// First attempt: normal remove
	cmd := exec.Command(
		"pnputil.exe",
		"/remove-device",
		instanceID,
	)

	out, err := cmd.CombinedOutput()

	if err == nil {
		return "normal", nil
	}

	text := string(out)
	lower := strings.ToLower(text)

	// Critical system device -> retry with /force
	if strings.Contains(lower, "critical") {
		cmd = exec.Command(
			"pnputil.exe",
			"/remove-device",
			instanceID,
			"/force",
		)

		out, err = cmd.CombinedOutput()

		if err == nil {
			return "forced", nil
		}

		return "forced", fmt.Errorf(
			"%v\n%s",
			err,
			strings.TrimSpace(string(out)),
		)
	}

	return "normal", fmt.Errorf(
		"%v\n%s",
		err,
		strings.TrimSpace(text),
	)
}

// ============================================================
// DEVICE UI
// ============================================================

func deviceIcon(class string) fyne.Resource {
	if strings.EqualFold(class, "Keyboard") {
		return theme.ComputerIcon()
	}

	return theme.RadioButtonIcon()
}

func deviceCard(d Device) fyne.CanvasObject {
	name := d.FriendlyName

	if name == "" {
		name = "Unknown device"
	}

	classLabel := widget.NewLabelWithStyle(
		d.Class,
		fyne.TextAlignLeading,
		fyne.TextStyle{
			Bold: true,
		},
	)

	nameLabel := widget.NewLabel(name)
	nameLabel.Wrapping = fyne.TextWrapWord

	idLabel := widget.NewLabel(d.InstanceID)
	idLabel.TextStyle = fyne.TextStyle{
		Monospace: true,
	}
	idLabel.Wrapping = fyne.TextWrapBreak

	icon := widget.NewIcon(
		deviceIcon(d.Class),
	)

	text := container.NewVBox(
		classLabel,
		nameLabel,
		idLabel,
	)

	left := container.NewVBox(
		icon,
		layout.NewSpacer(),
	)

	return widget.NewCard(
		"",
		"",
		container.NewBorder(
			nil,
			nil,
			left,
			nil,
			text,
		),
	)
}

func emptyCard(text string) fyne.CanvasObject {
	label := widget.NewLabel(text)

	label.Alignment = fyne.TextAlignCenter
	label.Wrapping = fyne.TextWrapWord

	return widget.NewCard(
		"",
		"",
		container.NewPadded(label),
	)
}

func (ui *UI) refreshDeviceLists() {
	if ui.pendingBox == nil || ui.personalBox == nil {
		return
	}

	ui.pendingBox.RemoveAll()

	if len(ui.initialDevices) == 0 {
		ui.pendingBox.Add(
			emptyCard(
				tr(
					ui.lang,
					"No existing keyboard or mouse detected.",
					"Không phát hiện bàn phím hoặc chuột có sẵn.",
				),
			),
		)
	} else {
		for _, d := range ui.initialDevices {
			ui.pendingBox.Add(
				deviceCard(d),
			)
		}
	}

	ui.personalBox.RemoveAll()

	if len(ui.personal) == 0 {
		ui.personalBox.Add(
			emptyCard(
				tr(
					ui.lang,
					"Waiting for your personal keyboard and mouse...",
					"Đang chờ bàn phím và chuột cá nhân...",
				),
			),
		)
	} else {
		for _, d := range ui.personal {
			ui.personalBox.Add(
				deviceCard(d),
			)
		}
	}

	ui.pendingBox.Refresh()
	ui.personalBox.Refresh()
}

// ============================================================
// LANGUAGE
// ============================================================

func (ui *UI) refreshLanguage() {
	// Prevent nil-pointer panic during initial UI creation
	if ui.titleLabel == nil ||
		ui.subtitleLabel == nil ||
		ui.statusLabel == nil ||
		ui.instructionLabel == nil ||
		ui.pendingTitle == nil ||
		ui.personalTitle == nil ||
		ui.resultTitle == nil ||
		ui.resultLabel == nil ||
		ui.warningLabel == nil ||
		ui.removeButton == nil ||
		ui.restartButton == nil ||
		ui.guideButton == nil ||
		ui.languageText == nil ||
		ui.themeText == nil {
		return
	}

	ui.titleLabel.SetText(
		"Gear Isolation",
	)

	ui.subtitleLabel.SetText(
		tr(
			ui.lang,
			"Temporarily remove the PC's existing keyboard and mouse while keeping your personal gear active.",
			"Tạm loại bỏ bàn phím và chuột có sẵn của máy trong khi vẫn giữ gear cá nhân hoạt động.",
		),
	)

	ui.pendingTitle.SetText(
		tr(
			ui.lang,
			"1 · EXISTING DEVICES",
			"1 · THIẾT BỊ CÓ SẴN",
		),
	)

	ui.personalTitle.SetText(
		tr(
			ui.lang,
			"2 · PERSONAL GEAR",
			"2 · GEAR CÁ NHÂN",
		),
	)

	ui.resultTitle.SetText(
		tr(
			ui.lang,
			"Activity",
			"Hoạt động",
		),
	)

	ui.languageText.SetText(
		tr(
			ui.lang,
			"Language",
			"Ngôn ngữ",
		),
	)

	ui.themeText.SetText(
		tr(
			ui.lang,
			"Theme",
			"Giao diện",
		),
	)

	ui.removeButton.SetText(
		tr(
			ui.lang,
			"Remove existing devices",
			"Loại bỏ thiết bị có sẵn",
		),
	)

	ui.restartButton.SetText(
		tr(
			ui.lang,
			"Restart detection",
			"Quét lại từ đầu",
		),
	)

	ui.guideButton.SetText(
		tr(
			ui.lang,
			"How to use",
			"Hướng dẫn",
		),
	)

	ui.warningLabel.SetText(
		tr(
			ui.lang,
			"Removed devices may return after reconnect, hardware rescan, or reboot.",
			"Thiết bị đã loại bỏ có thể trở lại sau khi cắm lại, quét phần cứng hoặc reboot.",
		),
	)

	ui.mu.Lock()

	scanning := ui.scanning
	removed := ui.removed

	personalReady :=
		hasClass(ui.personal, "Keyboard") &&
			hasClass(ui.personal, "Mouse")

	ui.mu.Unlock()

	switch {

	case removed:
		ui.statusLabel.SetText(
			tr(
				ui.lang,
				"✓ Isolation complete",
				"✓ Đã cách ly",
			),
		)

		ui.instructionLabel.SetText(
			tr(
				ui.lang,
				"Personal gear is active. Existing devices were removed from this Windows session.",
				"Gear cá nhân đang hoạt động. Thiết bị có sẵn đã bị loại khỏi phiên Windows hiện tại.",
			),
		)

	case scanning:
		ui.statusLabel.SetText(
			tr(
				ui.lang,
				"● Waiting for personal gear",
				"● Đang chờ gear cá nhân",
			),
		)

		ui.instructionLabel.SetText(
			tr(
				ui.lang,
				"Plug in your personal keyboard and mouse now.",
				"Hãy cắm bàn phím và chuột cá nhân ngay bây giờ.",
			),
		)

	case personalReady:
		ui.statusLabel.SetText(
			tr(
				ui.lang,
				"✓ Personal gear detected",
				"✓ Đã phát hiện gear cá nhân",
			),
		)

		ui.instructionLabel.SetText(
			tr(
				ui.lang,
				"Verify the new devices, then remove the existing devices.",
				"Kiểm tra gear mới rồi loại bỏ thiết bị có sẵn.",
			),
		)

	default:
		ui.statusLabel.SetText(
			tr(
				ui.lang,
				"● Initializing",
				"● Đang khởi tạo",
			),
		)

		ui.instructionLabel.SetText(
			tr(
				ui.lang,
				"Preparing device detection...",
				"Đang chuẩn bị quét thiết bị...",
			),
		)
	}

	ui.refreshDeviceLists()
}

// ============================================================
// THEME
// ============================================================

func (ui *UI) setTheme(value string) {
	switch value {

	case "Light":
		ui.app.Settings().SetTheme(
			&forcedTheme{
				variant: theme.VariantLight,
			},
		)

	case "Dark":
		ui.app.Settings().SetTheme(
			&forcedTheme{
				variant: theme.VariantDark,
			},
		)

	default:
		ui.app.Settings().SetTheme(
			theme.DefaultTheme(),
		)
	}
}

// ============================================================
// WELCOME / HOW-TO GUIDE
// ============================================================

func guideBodyLabel(text string) *widget.Label {
	label := widget.NewLabel(text)
	label.Wrapping = fyne.TextWrapWord
	return label
}

func guideStepCard(step, title, body string) fyne.CanvasObject {
	stepLabel := widget.NewLabelWithStyle(
		step,
		fyne.TextAlignCenter,
		fyne.TextStyle{Bold: true},
	)

	titleLabel := widget.NewLabelWithStyle(
		title,
		fyne.TextAlignCenter,
		fyne.TextStyle{Bold: true},
	)
	titleLabel.Wrapping = fyne.TextWrapWord

	bodyLabel := widget.NewLabel(body)
	bodyLabel.Alignment = fyne.TextAlignCenter
	bodyLabel.Wrapping = fyne.TextWrapWord

	return widget.NewCard(
		"",
		"",
		container.NewVBox(
			stepLabel,
			widget.NewSeparator(),
			titleLabel,
			bodyLabel,
		),
	)
}

func (ui *UI) showGuide() {
	// Keep the guide concise and scannable. The detailed device IDs remain
	// on the main screen; this modal only explains the safe workflow.

	title := widget.NewLabelWithStyle(
		"Gear Isolation",
		fyne.TextAlignLeading,
		fyne.TextStyle{Bold: true},
	)

	tagline := guideBodyLabel(
		tr(
			ui.lang,
			"Temporarily isolate the gaming café's keyboard and mouse so your personal gear can be used without faulty clicks, ghost input, or other unwanted input.",
			"Tạm thời cách ly bàn phím và chuột của máy net để bạn dùng gear cá nhân ổn định hơn, không bị ảnh hưởng bởi tự click, double-click lỗi, ghost input hoặc input ngoài ý muốn từ thiết bị của quán.",
		),
	)

	benefits := widget.NewLabel(
		tr(
			ui.lang,
			"✓ Avoid faulty clicks     ✓ Stop ghost input     ✓ Keep personal gear active",
			"✓ Tránh click lỗi     ✓ Ngăn ghost input     ✓ Giữ gear cá nhân hoạt động",
		),
	)
	benefits.Alignment = fyne.TextAlignCenter
	benefits.TextStyle = fyne.TextStyle{Bold: true}
	benefits.Wrapping = fyne.TextWrapWord

	important := widget.NewCard(
		tr(ui.lang, "⚠ IMPORTANT", "⚠ QUAN TRỌNG"),
		"",
		guideBodyLabel(
			tr(
				ui.lang,
				"Do NOT connect your personal keyboard or mouse before starting. Gear Isolation must first record the devices that already belong to the PC.",
				"CHƯA cắm bàn phím hoặc chuột cá nhân trước khi bắt đầu. Gear Isolation cần ghi nhận phím chuột có sẵn của máy net trước để không nhận nhầm gear cá nhân.",
			),
		),
	)

	step1 := guideStepCard(
		"1",
		tr(ui.lang, "Keep café devices connected", "Giữ nguyên thiết bị máy net"),
		tr(
			ui.lang,
			"Leave the café keyboard and mouse connected. Keep your personal gear unplugged.",
			"Để nguyên phím chuột của máy net đang kết nối. Chưa cắm gear cá nhân.",
		),
	)

	step2 := guideStepCard(
		"2",
		tr(ui.lang, "Scan existing devices", "Quét thiết bị có sẵn"),
		tr(
			ui.lang,
			"Press Start. The app records the keyboard and mouse already connected to the PC.",
			"Nhấn Bắt đầu. Ứng dụng sẽ ghi nhận bàn phím và chuột đang có sẵn trên máy.",
		),
	)

	step3 := guideStepCard(
		"3",
		tr(ui.lang, "Connect personal gear", "Cắm gear cá nhân"),
		tr(
			ui.lang,
			"When prompted, plug in your keyboard and mouse. They will appear under Personal Gear.",
			"Khi ứng dụng yêu cầu, hãy cắm bàn phím và chuột cá nhân. Chúng sẽ xuất hiện ở cột Gear cá nhân.",
		),
	)

	step4 := guideStepCard(
		"4",
		tr(ui.lang, "Verify and isolate", "Kiểm tra và cách ly"),
		tr(
			ui.lang,
			"Check both lists, then click Remove existing devices. Only the original café devices are removed.",
			"Kiểm tra hai danh sách rồi nhấn Loại bỏ thiết bị có sẵn. Chỉ phím chuột ban đầu của máy net bị loại bỏ.",
		),
	)

	steps := container.NewGridWithColumns(
		4,
		step1,
		step2,
		step3,
		step4,
	)

	note := widget.NewCard(
		tr(ui.lang, "Notes", "Lưu ý"),
		"",
		guideBodyLabel(
			tr(
				ui.lang,
				"• Gaming keyboards and mice can appear as multiple Keyboard/Mouse interfaces. This is normal.\n• Gear Isolation does not uninstall drivers.\n• Removed devices may return after reconnecting USB hardware, a hardware rescan, or reboot.",
				"• Bàn phím/chuột gaming có thể xuất hiện thành nhiều interface Keyboard/Mouse. Đây là bình thường.\n• Gear Isolation không gỡ driver.\n• Thiết bị đã loại bỏ có thể hoạt động lại sau khi cắm/rút USB, Windows quét lại phần cứng hoặc reboot.",
			),
		),
	)

	var d dialog.Dialog

	startButton := NewPointerButton(
		tr(ui.lang, "START GEAR ISOLATION", "BẮT ĐẦU GEAR ISOLATION"),
		theme.MediaPlayIcon(),
		func() {
			if d != nil {
				d.Hide()
			}
			ui.startDetection()
		},
	)
	startButton.Importance = widget.HighImportance

	closeButton := NewPointerTextButton(
		tr(ui.lang, "Not now", "Để sau"),
		func() {
			if d != nil {
				d.Hide()
			}
		},
	)

	actions := container.NewBorder(
		nil,
		nil,
		closeButton,
		startButton,
		nil,
	)

	content := container.NewVBox(
		title,
		tagline,
		benefits,
		widget.NewSeparator(),
		important,
		widget.NewLabelWithStyle(
			tr(ui.lang, "4 SIMPLE STEPS", "4 BƯỚC ĐƠN GIẢN"),
			fyne.TextAlignLeading,
			fyne.TextStyle{Bold: true},
		),
		steps,
		note,
		widget.NewSeparator(),
		actions,
	)

	scroll := container.NewVScroll(
		container.NewPadded(content),
	)
	scroll.SetMinSize(fyne.NewSize(1040, 590))

	d = dialog.NewCustomWithoutButtons(
		tr(ui.lang, "Welcome to Gear Isolation", "Chào mừng đến với Gear Isolation"),
		scroll,
		ui.window,
	)
	d.Resize(fyne.NewSize(1180, 720))
	d.Show()
}

// ============================================================
// INITIAL SNAPSHOT
// ============================================================

func (ui *UI) startDetection() {
	ui.mu.Lock()

	if ui.scanning {
		ui.mu.Unlock()
		return
	}

	ui.scanning = true
	ui.removed = false

	ui.personal = nil

	ui.mu.Unlock()

	ui.removeButton.Disable()
	ui.restartButton.Disable()

	ui.resultLabel.SetText(
		tr(
			ui.lang,
			"Reading existing input devices...",
			"Đang đọc thiết bị nhập có sẵn...",
		),
	)

	ui.statusLabel.SetText(
		tr(
			ui.lang,
			"● Creating initial snapshot",
			"● Đang tạo snapshot ban đầu",
		),
	)

	ui.instructionLabel.SetText(
		tr(
			ui.lang,
			"Do not connect your personal gear until the snapshot is complete.",
			"Chưa cắm gear cá nhân cho đến khi snapshot hoàn tất.",
		),
	)

	go func() {
		devices, err := getInputDevices()

		if err != nil {
			fyne.Do(func() {
				ui.mu.Lock()

				ui.scanning = false

				ui.mu.Unlock()

				ui.statusLabel.SetText(
					tr(
						ui.lang,
						"✕ Device scan failed",
						"✕ Quét thiết bị thất bại",
					),
				)

				ui.resultLabel.SetText(
					err.Error(),
				)

				ui.restartButton.Enable()
			})

			return
		}

		fyne.Do(func() {
			ui.mu.Lock()

			ui.initialDevices = devices
			ui.initialMap = deviceMap(devices)
			ui.personal = nil

			ui.mu.Unlock()

			ui.refreshDeviceLists()

			ui.statusLabel.SetText(
				tr(
					ui.lang,
					"● Waiting for personal gear",
					"● Đang chờ gear cá nhân",
				),
			)

			ui.instructionLabel.SetText(
				tr(
					ui.lang,
					"Plug in your personal keyboard and mouse now.",
					"Hãy cắm bàn phím và chuột cá nhân ngay bây giờ.",
				),
			)

			ui.resultLabel.SetText(
				tr(
					ui.lang,
					"Initial snapshot complete. Waiting for new devices...",
					"Snapshot ban đầu hoàn tất. Đang chờ thiết bị mới...",
				),
			)
		})

		ui.detectPersonalGear()
	}()
}

// ============================================================
// DETECT PERSONAL GEAR
// ============================================================

func (ui *UI) detectPersonalGear() {
	for {
		time.Sleep(
			1 * time.Second,
		)

		ui.mu.Lock()

		if !ui.scanning {
			ui.mu.Unlock()
			return
		}

		initialMap := ui.initialMap

		ui.mu.Unlock()

		current, err := getInputDevices()

		if err != nil {
			continue
		}

		added := findNewDevices(
			initialMap,
			current,
		)

		fyne.Do(func() {
			ui.mu.Lock()

			ui.personal = added

			ui.mu.Unlock()

			ui.refreshDeviceLists()
		})

		if hasClass(added, "Keyboard") &&
			hasClass(added, "Mouse") {

			fyne.Do(func() {
				ui.mu.Lock()

				ui.personal = added
				ui.scanning = false

				ui.mu.Unlock()

				ui.statusLabel.SetText(
					tr(
						ui.lang,
						"✓ Personal gear detected",
						"✓ Đã phát hiện gear cá nhân",
					),
				)

				ui.instructionLabel.SetText(
					tr(
						ui.lang,
						"Verify the new devices, then click Remove existing devices.",
						"Kiểm tra gear mới rồi nhấn Loại bỏ thiết bị có sẵn.",
					),
				)

				ui.resultLabel.SetText(
					fmt.Sprintf(
						tr(
							ui.lang,
							"Detected %d new device interfaces.",
							"Đã phát hiện %d interface thiết bị mới.",
						),
						len(added),
					),
				)

				ui.removeButton.Enable()
				ui.restartButton.Enable()

				ui.refreshDeviceLists()
			})

			return
		}
	}
}

// ============================================================
// REMOVE EXISTING DEVICES
// ============================================================

func (ui *UI) removeExistingDevices() {
	ui.removeButton.Disable()
	ui.restartButton.Disable()

	ui.statusLabel.SetText(
		tr(
			ui.lang,
			"● Removing existing devices...",
			"● Đang loại bỏ thiết bị có sẵn...",
		),
	)

	ui.resultLabel.SetText(
		tr(
			ui.lang,
			"Starting removal...",
			"Bắt đầu loại bỏ...",
		),
	)

	go func() {
		current, err := getInputDevices()

		if err != nil {
			fyne.Do(func() {
				ui.statusLabel.SetText(
					tr(
						ui.lang,
						"✕ Failed to read devices",
						"✕ Không thể đọc thiết bị",
					),
				)

				ui.resultLabel.SetText(
					err.Error(),
				)

				ui.removeButton.Enable()
				ui.restartButton.Enable()
			})

			return
		}

		currentMap := deviceMap(current)

		ui.mu.Lock()

		initialMap := ui.initialMap

		ui.mu.Unlock()

		success := 0
		failed := 0
		skipped := 0

		var logs []string

		for id, d := range initialMap {

			if _, connected := currentMap[id]; !connected {
				skipped++

				logs = append(
					logs,
					fmt.Sprintf(
						"SKIP   %-8s  %s",
						d.Class,
						d.FriendlyName,
					),
				)

				continue
			}

			mode, err := removeDevice(
				d.InstanceID,
			)

			if err != nil {
				failed++

				logs = append(
					logs,
					fmt.Sprintf(
						"FAIL   %-8s  %s\n%s",
						d.Class,
						d.FriendlyName,
						err.Error(),
					),
				)

				continue
			}

			success++

			if mode == "forced" {
				logs = append(
					logs,
					fmt.Sprintf(
						"OK     %-8s  %s  [forced]",
						d.Class,
						d.FriendlyName,
					),
				)
			} else {
				logs = append(
					logs,
					fmt.Sprintf(
						"OK     %-8s  %s",
						d.Class,
						d.FriendlyName,
					),
				)
			}
		}

		fyne.Do(func() {
			ui.mu.Lock()

			ui.removed = true

			ui.mu.Unlock()

			if failed == 0 {
				ui.statusLabel.SetText(
					tr(
						ui.lang,
						"✓ Isolation complete",
						"✓ Cách ly hoàn tất",
					),
				)
			} else {
				ui.statusLabel.SetText(
					tr(
						ui.lang,
						"⚠ Isolation completed with errors",
						"⚠ Hoàn tất nhưng có lỗi",
					),
				)
			}

			ui.instructionLabel.SetText(
				tr(
					ui.lang,
					"Personal gear remains active. Existing devices may return after reconnect, hardware rescan, or reboot.",
					"Gear cá nhân vẫn hoạt động. Thiết bị cũ có thể trở lại sau khi cắm lại, quét phần cứng hoặc reboot.",
				),
			)

			summary := fmt.Sprintf(
				tr(
					ui.lang,
					"Removed: %d     Failed: %d     Skipped: %d",
					"Đã loại bỏ: %d     Lỗi: %d     Bỏ qua: %d",
				),
				success,
				failed,
				skipped,
			)

			if len(logs) > 0 {
				summary += "\n\n" + strings.Join(
					logs,
					"\n",
				)
			}

			ui.resultLabel.SetText(
				summary,
			)

			/*
				Do NOT automatically rescan after removal.

				Restart detection also remains disabled after
				successful removal because triggering hardware
				changes may allow removed devices to enumerate again.
			*/

			ui.removeButton.Disable()
			ui.restartButton.Disable()
		})
	}()
}

// ============================================================
// MAIN
// ============================================================

func main() {
	a := app.NewWithID(
		"com.gearisolation.app",
	)

	w := a.NewWindow(
		"Gear Isolation",
	)

	/*
		Optimized for 1600x900.

		Leaves enough room for:
		- Windows taskbar
		- window borders
		- desktop scaling
	*/

	w.Resize(
		fyne.NewSize(
			1600,
			900,
		),
	)

	w.SetMaster()

	ui := &UI{
		app:    a,
		window: w,
		lang:   Vietnamese,
	}

	// ========================================================
	// HEADER
	// ========================================================

	ui.titleLabel = widget.NewLabelWithStyle(
		"Gear Isolation",
		fyne.TextAlignLeading,
		fyne.TextStyle{
			Bold: true,
		},
	)

	ui.subtitleLabel = widget.NewLabel("")
	ui.subtitleLabel.Wrapping = fyne.TextWrapWord

	ui.languageText = widget.NewLabel("Language")
	ui.themeText = widget.NewLabel("Theme")

	ui.languageSelect = widget.NewSelect(
		[]string{
			"English",
			"Tiếng Việt",
		},
		func(value string) {
			if value == "Tiếng Việt" {
				ui.lang = Vietnamese
			} else {
				ui.lang = English
			}

			ui.refreshLanguage()
		},
	)

	ui.themeSelect = widget.NewSelect(
		[]string{
			"System",
			"Light",
			"Dark",
		},
		func(value string) {
			ui.setTheme(value)
		},
	)

	/*
		IMPORTANT:
		headerLeft nằm ở CENTER chứ không phải LEFT.
		Như vậy subtitle có toàn bộ khoảng trống còn lại.
	*/
	headerLeft := container.NewVBox(
		ui.titleLabel,
		ui.subtitleLabel,
	)

	languageGroup := container.NewVBox(
		ui.languageText,
		ui.languageSelect,
	)

	themeGroup := container.NewVBox(
		ui.themeText,
		ui.themeSelect,
	)

	settingsBox := container.NewHBox(
		languageGroup,
		widget.NewSeparator(),
		themeGroup,
	)

	header := container.NewBorder(
		nil,
		nil,
		nil,
		settingsBox,
		headerLeft,
	)

	// ========================================================
	// STATUS
	// ========================================================

	ui.statusLabel = widget.NewLabelWithStyle(
		"",
		fyne.TextAlignLeading,
		fyne.TextStyle{
			Bold: true,
		},
	)

	ui.instructionLabel = widget.NewLabel("")
	ui.instructionLabel.Wrapping = fyne.TextWrapWord

	statusCard := widget.NewCard(
		"",
		"",
		container.NewVBox(
			ui.statusLabel,
			ui.instructionLabel,
		),
	)

	// ========================================================
	// DEVICE LISTS
	// ========================================================

	ui.pendingTitle = widget.NewLabelWithStyle(
		"",
		fyne.TextAlignLeading,
		fyne.TextStyle{
			Bold: true,
		},
	)

	ui.personalTitle = widget.NewLabelWithStyle(
		"",
		fyne.TextAlignLeading,
		fyne.TextStyle{
			Bold: true,
		},
	)

	ui.pendingBox = container.NewVBox()
	ui.personalBox = container.NewVBox()

	pendingScroll := container.NewVScroll(
		ui.pendingBox,
	)

	personalScroll := container.NewVScroll(
		ui.personalBox,
	)

	pendingPanel := widget.NewCard(
		"",
		"",
		container.NewBorder(
			ui.pendingTitle,
			nil,
			nil,
			nil,
			pendingScroll,
		),
	)

	personalPanel := widget.NewCard(
		"",
		"",
		container.NewBorder(
			ui.personalTitle,
			nil,
			nil,
			nil,
			personalScroll,
		),
	)

	/*
		50/50 landscape.
	*/
	deviceSplit := container.NewHSplit(
		pendingPanel,
		personalPanel,
	)

	deviceSplit.Offset = 0.5

	// ========================================================
	// ACTIVITY LOG
	// ========================================================

	ui.resultTitle = widget.NewLabelWithStyle(
		"",
		fyne.TextAlignLeading,
		fyne.TextStyle{
			Bold: true,
		},
	)

	ui.resultLabel = widget.NewLabel("")

	ui.resultLabel.TextStyle = fyne.TextStyle{
		Monospace: true,
	}

	ui.resultLabel.Wrapping = fyne.TextWrapWord

	resultScroll := container.NewVScroll(
		ui.resultLabel,
	)

	/*
		Activity chỉ chiếm khoảng 100px.
		Không ăn mất không gian của device list.
	*/
	resultScroll.SetMinSize(
		fyne.NewSize(
			0,
			95,
		),
	)

	resultCard := widget.NewCard(
		"",
		"",
		container.NewBorder(
			ui.resultTitle,
			nil,
			nil,
			nil,
			resultScroll,
		),
	)

	// ========================================================
	// WARNING + ACTIONS
	// ========================================================

	ui.warningLabel = widget.NewLabel("")
	ui.warningLabel.Wrapping = fyne.TextWrapWord

	warning := container.NewHBox(
		widget.NewIcon(
			theme.WarningIcon(),
		),
		ui.warningLabel,
	)

	ui.guideButton = NewPointerTextButton(
		"",
		func() {
			ui.showGuide()
		},
	)

	ui.restartButton = NewPointerButton(
		"",
		theme.ViewRefreshIcon(),
		func() {
			ui.startDetection()
		},
	)

	ui.removeButton = NewPointerButton(
		"",
		theme.DeleteIcon(),
		func() {
			ui.removeExistingDevices()
		},
	)

	ui.removeButton.Importance = widget.HighImportance
	ui.removeButton.Disable()

	actions := container.NewHBox(
		ui.guideButton,
		ui.restartButton,
		ui.removeButton,
	)

	footer := container.NewBorder(
		nil,
		nil,
		warning,
		actions,
		nil,
	)

	// ========================================================
	// LANDSCAPE LAYOUT
	// ========================================================

	/*
		Top cực gọn:
		- header
		- status

		Middle:
		- 2 panel device chiếm toàn bộ phần dư

		Bottom:
		- activity ~95 px
		- warning + buttons
	*/

	topArea := container.NewVBox(
		header,
		statusCard,
	)

	bottomArea := container.NewVBox(
		resultCard,
		footer,
	)

	root := container.NewBorder(
		topArea,
		bottomArea,
		nil,
		nil,
		deviceSplit,
	)

	w.SetContent(
		container.NewPadded(
			root,
		),
	)

	/*
		Now every widget exists.

		It is safe for SetSelected() to trigger callbacks.
	*/

	ui.languageSelect.SetSelected(
		"Tiếng Việt",
	)

	ui.themeSelect.SetSelected(
		"System",
	)

	ui.refreshLanguage()
	ui.refreshDeviceLists()

	// ========================================================
	// WELCOME GUIDE FIRST - DO NOT SNAPSHOT YET
	// ========================================================

	go func() {
		time.Sleep(300 * time.Millisecond)

		fyne.Do(func() {
			ui.showGuide()
		})
	}()

	w.CenterOnScreen()

	w.ShowAndRun()
}
