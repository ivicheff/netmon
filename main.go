package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"fyne.io/systray"
)

// ---------- настройки ----------

var (
	proxyFlag = flag.String("proxy", "auto", "прокси: auto (системный), none, либо адрес http://127.0.0.1:10809 / socks5://127.0.0.1:10808")
	host      = flag.String("host", "1.1.1.1:443", "адрес для прямой проверки связи (host:port)")
	interval  = flag.Duration("i", time.Second, "интервал проверки связи")
	every     = flag.Duration("speed-every", 2*time.Minute, "как часто мерить скорость (замер нагружает канал!)")
	sizeMB    = flag.Int("size", 5, "размер тестовой загрузки, МБ")
	noUp      = flag.Bool("no-up", false, "не мерить скорость отдачи")
	slowPing  = flag.Int("slow-ping", 150, "прямой пинг (мс), выше которого значок жёлтый")
	slowLoss  = flag.Float64("slow-loss", 3, "потери (%), выше которых значок жёлтый")
	procsFlag = flag.Bool("procs", false, "служебный: окно со списком программ и их соединений")
	consoleFlag = flag.Bool("console", false, "служебный: подробный вывод в консольном окне")
	slowSpeed = flag.Float64("slow-speed", 5, "загрузка (Мбит/с), ниже которой значок жёлтый")
)

const (
	green  = "\033[32m"
	red    = "\033[31m"
	yellow = "\033[33m"
	cyan   = "\033[36m"
	gray   = "\033[90m"
	reset  = "\033[0m"
)

const probeURL = "http://cp.cloudflare.com/generate_204"

// ---------- данные ----------

type sample struct {
	ok  bool
	rtt time.Duration
}

type outage struct {
	start time.Time
	dur   time.Duration
}

type path struct {
	name   string
	client *http.Client // для замеров скорости

	samples []sample
	ok      bool
	rtt     time.Duration
	reason  string

	consecFail  int
	lastOK      time.Time
	failStart   time.Time
	inOutage    bool
	outageStart time.Time
	events      []outage

	down, up float64
	hasSpeed bool
	speedAt  time.Time
}

var (
	mu       sync.Mutex
	direct   *path
	viaProxy *path
	proxyURL *url.URL
	busy     bool
	logFile  *os.File
	logPath  string
)

func logf(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	if *consoleFlag { // в консольном окне события печатаются сразу
		col := cyan
		switch {
		case strings.Contains(msg, "ОБРЫВ"):
			col = red
		case strings.Contains(msg, "вернулась"):
			col = green
		}
		fmt.Printf("%s  %s%s%s\n", time.Now().Format("15:04:05"), col, msg, reset)
		return
	}
	if logFile == nil {
		return
	}
	fmt.Fprintf(logFile, "%s  %s\n", time.Now().Format("2006-01-02 15:04:05"), msg)
}

// record вызывается под mu.
func (p *path) record(ok bool, rtt time.Duration, reason string, now time.Time) {
	p.samples = append(p.samples, sample{ok, rtt})
	if len(p.samples) > 60 {
		p.samples = p.samples[len(p.samples)-60:]
	}
	p.ok, p.rtt, p.reason = ok, rtt, reason

	if !ok {
		p.consecFail++
		if p.consecFail == 1 {
			p.failStart = now
		}
		if p.consecFail == 2 && !p.inOutage { // два промаха подряд = обрыв
			p.inOutage = true
			p.outageStart = p.failStart
			logf("[%s] ОБРЫВ: %s", p.name, reason)
		}
		return
	}
	p.lastOK = now
	if p.inOutage {
		d := now.Sub(p.outageStart)
		p.events = append(p.events, outage{p.outageStart, d})
		if len(p.events) > 100 {
			p.events = p.events[1:]
		}
		logf("[%s] связь вернулась, обрыв длился %s", p.name, d.Round(time.Second))
		p.inOutage = false
	}
	p.consecFail = 0
}

type stats struct {
	n                      int
	loss, avg, jitter, max float64
}

func (p *path) stats() stats {
	var s stats
	var prev, sum, jsum float64
	var havePrev bool
	var succ, jn int
	for _, x := range p.samples {
		s.n++
		if !x.ok {
			continue
		}
		ms := float64(x.rtt.Microseconds()) / 1000
		succ++
		sum += ms
		if ms > s.max {
			s.max = ms
		}
		if havePrev {
			jsum += math.Abs(ms - prev)
			jn++
		}
		prev, havePrev = ms, true
	}
	if s.n > 0 {
		s.loss = float64(s.n-succ) * 100 / float64(s.n)
	}
	if succ > 0 {
		s.avg = sum / float64(succ)
	}
	if jn > 0 {
		s.jitter = jsum / float64(jn)
	}
	return s
}

func (p *path) outageLine() string {
	cnt := 0
	var last *outage
	cutoff := time.Now().Add(-time.Hour)
	for i := range p.events {
		if p.events[i].start.After(cutoff) {
			cnt++
			last = &p.events[i]
		}
	}
	if p.inOutage {
		return fmt.Sprintf("Обрывов за час: %d, сейчас обрыв уже %s", cnt+1, time.Since(p.outageStart).Round(time.Second))
	}
	if last == nil {
		return "Обрывов за час: нет"
	}
	return fmt.Sprintf("Обрывов за час: %d, последний в %s (%s)", cnt, last.start.Format("15:04:05"), last.dur.Round(time.Second))
}

// ---------- проверки ----------

func probeDirect() (bool, time.Duration, string) {
	start := time.Now()
	c, err := net.DialTimeout("tcp", *host, 2*time.Second)
	if err != nil {
		return false, 0, "нет связи с интернетом (напрямую)"
	}
	c.Close()
	return true, time.Since(start), ""
}

var probeClient *http.Client

func probeProxy() (bool, time.Duration, string) {
	pc, err := net.DialTimeout("tcp", proxyURL.Host, 700*time.Millisecond)
	if err != nil {
		return false, 0, "порт прокси закрыт — Xray/клиент не запущен?"
	}
	pc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", probeURL, nil)
	start := time.Now()
	resp, err := probeClient.Do(req)
	if err != nil {
		return false, 0, "прокси жив, но интернет через него не идёт"
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return false, 0, fmt.Sprintf("прокси вернул ошибку %d", resp.StatusCode)
	}
	return true, time.Since(start), ""
}

// ---------- скорость ----------

func measureDown(ctx context.Context, c *http.Client, size int) (float64, error) {
	url := fmt.Sprintf("https://speed.cloudflare.com/__down?bytes=%d", size)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	start := time.Now()
	n, err := io.Copy(io.Discard, resp.Body)
	el := time.Since(start).Seconds()
	if err != nil && n == 0 {
		return 0, err
	}
	if el <= 0 {
		el = 0.001
	}
	return float64(n) * 8 / 1e6 / el, nil
}

func measureUp(ctx context.Context, c *http.Client, size int) (float64, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://speed.cloudflare.com/__up",
		bytes.NewReader(make([]byte, size)))
	req.Header.Set("Content-Type", "application/octet-stream")
	start := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return float64(size) * 8 / 1e6 / time.Since(start).Seconds(), nil
}

func runSpeedTest() {
	mu.Lock()
	if busy {
		mu.Unlock()
		return
	}
	busy = true
	mu.Unlock()
	update()
	logf("замер скорости начался")

	for _, p := range []*path{direct, viaProxy} {
		if p == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		d, err := measureDown(ctx, p.client, *sizeMB*1_000_000)
		var u float64
		if err == nil && !*noUp {
			u, _ = measureUp(ctx, p.client, *sizeMB*1_000_000/4)
		}
		cancel()
		if err == nil {
			mu.Lock()
			p.down, p.up, p.hasSpeed, p.speedAt = d, u, true, time.Now()
			mu.Unlock()
			logf("[%s] скорость: загрузка %.1f, отдача %.1f Мбит/с", p.name, d, u)
		}
		update()
	}

	mu.Lock()
	busy = false
	mu.Unlock()
	update()
}

// ---------- значок ----------

func makeIcon(c color.RGBA) []byte {
	const n = 32
	img := image.NewRGBA(image.Rect(0, 0, n, n))
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			d := math.Hypot(float64(x)-15.5, float64(y)-15.5)
			a := math.Max(0, math.Min(1, 14.5-d))
			if a > 0 {
				img.SetRGBA(x, y, color.RGBA{
					uint8(float64(c.R) * a), uint8(float64(c.G) * a), uint8(float64(c.B) * a), uint8(255 * a)})
			}
		}
	}
	var p bytes.Buffer
	png.Encode(&p, img)

	var out bytes.Buffer
	binary.Write(&out, binary.LittleEndian, []uint16{0, 1, 1})
	out.Write([]byte{n, n, 0, 0})
	binary.Write(&out, binary.LittleEndian, uint16(1))
	binary.Write(&out, binary.LittleEndian, uint16(32))
	binary.Write(&out, binary.LittleEndian, uint32(p.Len()))
	binary.Write(&out, binary.LittleEndian, uint32(22))
	out.Write(p.Bytes())
	return out.Bytes()
}

var icons = [4][]byte{
	makeIcon(color.RGBA{46, 204, 113, 255}), // 0 зелёный — всё хорошо
	makeIcon(color.RGBA{241, 196, 15, 255}), // 1 жёлтый — нестабильно
	makeIcon(color.RGBA{230, 126, 34, 255}), // 2 оранжевый — не работает прокси
	makeIcon(color.RGBA{231, 76, 60, 255}),  // 3 красный — нет интернета
}

// ---------- текст ----------

func fmtSpeed(v float64) string {
	if v >= 1000 {
		return fmt.Sprintf("%.2f Гбит/с", v/1000)
	}
	return fmt.Sprintf("%.1f Мбит/с", v)
}

func pathLine(p *path) string {
	if !p.ok {
		return "НЕТ ОТВЕТА"
	}
	s := p.stats()
	return fmt.Sprintf("%d мс · потери %.0f%% · джиттер %.0f мс · макс %.0f мс", p.rtt.Milliseconds(), s.loss, s.jitter, s.max)
}

func speedLine(p *path, isBusy bool) string {
	if !p.hasSpeed {
		if isBusy {
			return "   скорость: измеряю…"
		}
		return "   скорость: ещё не измерена"
	}
	s := "   скорость: загрузка " + fmtSpeed(p.down)
	if p.up > 0 {
		s += " · отдача " + fmtSpeed(p.up)
	}
	if isBusy {
		s += " (обновляю…)"
	}
	return s
}

func degraded(p *path, checkPing bool) bool {
	if !p.ok {
		return false
	}
	if checkPing && p.rtt > time.Duration(*slowPing)*time.Millisecond {
		return true
	}
	if p.stats().loss > *slowLoss {
		return true
	}
	return p.hasSpeed && p.down < *slowSpeed
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// ---------- трей ----------

var (
	mStatus, mD, mDS, mDO, mP, mPS, mPO *systray.MenuItem
	lastLevel                          = -1
)

func refresh() {
	mu.Lock()
	level := 0
	status := "Всё в порядке"
	switch {
	case !direct.ok && direct.consecFail > 0:
		level, status = 3, "Нет связи с интернетом"
	case viaProxy != nil && !viaProxy.ok && viaProxy.consecFail > 0:
		level, status = 2, "Прокси не работает: "+viaProxy.reason
	case degraded(direct, true) || (viaProxy != nil && degraded(viaProxy, false)):
		level, status = 1, "Нестабильно"
	}

	dLine := direct.name + ": " + pathLine(direct)
	dSpeed := speedLine(direct, busy)
	dOut := "   " + direct.outageLine()
	var pLine, pSpeed, pOut string
	tip := status + "\nНапрямую: " + shortLine(direct)
	if viaProxy != nil {
		pLine = viaProxy.name + ": " + pathLine(viaProxy)
		pSpeed = speedLine(viaProxy, busy)
		pOut = "   " + viaProxy.outageLine()
		tip += "\nПрокси: " + shortLine(viaProxy)
	}
	mu.Unlock()

	if level != lastLevel {
		systray.SetIcon(icons[level])
		lastLevel = level
	}
	systray.SetTooltip(truncate(tip, 120))
	mStatus.SetTitle(status)
	mD.SetTitle(dLine)
	mDS.SetTitle(dSpeed)
	mDO.SetTitle(dOut)
	if viaProxy != nil {
		mP.SetTitle(pLine)
		mPS.SetTitle(pSpeed)
		mPO.SetTitle(pOut)
	}
}

func shortLine(p *path) string {
	if !p.ok {
		return "нет ответа"
	}
	return fmt.Sprintf("%d мс, потери %.0f%%", p.rtt.Milliseconds(), p.stats().loss)
}

func monitor() {
	var lastSpeed time.Time
	tick := time.NewTicker(*interval)
	defer tick.Stop()
	for ; ; <-tick.C {
		var wg sync.WaitGroup
		var dOK, pOK bool
		var dRTT, pRTT time.Duration
		var dWhy, pWhy string

		wg.Add(1)
		go func() { defer wg.Done(); dOK, dRTT, dWhy = probeDirect() }()
		if viaProxy != nil {
			wg.Add(1)
			go func() { defer wg.Done(); pOK, pRTT, pWhy = probeProxy() }()
		}
		wg.Wait()

		now := time.Now()
		mu.Lock()
		direct.record(dOK, dRTT, dWhy, now)
		if viaProxy != nil {
			viaProxy.record(pOK, pRTT, pWhy, now)
		}
		canSpeed := direct.ok
		mu.Unlock()
		tickUpdate()

		if canSpeed && time.Since(lastSpeed) >= *every {
			lastSpeed = time.Now()
			go runSpeedTest()
		}
	}
}

func onReady() {
	systray.SetTitle("Netmon")
	systray.SetIcon(icons[3])
	systray.SetTooltip("Проверяю сеть…")

	mStatus = systray.AddMenuItem("Проверяю…", "")
	mStatus.Disable()
	systray.AddSeparator()
	mD = systray.AddMenuItem("Напрямую: …", "Проверка: TCP-подключение к "+*host+" мимо прокси")
	mD.Disable()
	mDS = systray.AddMenuItem("", "")
	mDS.Disable()
	mDO = systray.AddMenuItem("", "")
	mDO.Disable()
	systray.AddSeparator()
	mP = systray.AddMenuItem("", "Проверка: HTTP-запрос к cp.cloudflare.com через прокси")
	mP.Disable()
	mPS = systray.AddMenuItem("", "")
	mPS.Disable()
	mPO = systray.AddMenuItem("", "")
	mPO.Disable()
	if viaProxy == nil {
		mP.SetTitle("Прокси не найден (включи системный прокси или задай -proxy)")
		mPS.Hide()
		mPO.Hide()
	}
	systray.AddSeparator()
	mHelp := systray.AddMenuItem("Пинг — время ответа; потери и джиттер — за последние 60 проверок", "")
	mHelp.Disable()
	systray.AddSeparator()
	mTest := systray.AddMenuItem("Измерить скорость сейчас", "")
	mCons := systray.AddMenuItem("Открыть консоль (подробно)", "")
	mProcs := systray.AddMenuItem("Какие программы используют сеть", "")
	mLog := systray.AddMenuItem("Открыть лог-файл (блокнот)", "")
	mQuit := systray.AddMenuItem("Выход", "")

	go monitor()
	go func() {
		for {
			select {
			case <-mTest.ClickedCh:
				go runSpeedTest()
			case <-mCons.ClickedCh:
				openConsole("-console")
			case <-mProcs.ClickedCh:
				openConsole("-procs")
			case <-mLog.ClickedCh:
				if logPath != "" {
					exec.Command("notepad.exe", logPath).Start()
				}
			case <-mQuit.ClickedCh:
				killConsole()
				systray.Quit()
				return
			}
		}
	}()
}

// "127.0.0.1:10809" или "http=...;https=...;socks=..." -> URL
func parseProxy(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "=") {
		return "http://" + s
	}
	m := map[string]string{}
	for _, part := range strings.Split(s, ";") {
		if kv := strings.SplitN(part, "=", 2); len(kv) == 2 {
			m[strings.ToLower(strings.TrimSpace(kv[0]))] = strings.TrimSpace(kv[1])
		}
	}
	for _, k := range []string{"http", "https"} {
		if v := m[k]; v != "" {
			return "http://" + v
		}
	}
	if v := m["socks"]; v != "" {
		return "socks5://" + v
	}
	return ""
}

func update() {
	if !*consoleFlag {
		refresh()
	}
}

func tickUpdate() {
	if *consoleFlag {
		printLine()
	} else {
		refresh()
	}
}

// ---------- консольное окно ----------

var (
	consoleMu   sync.Mutex
	consoleCmds = map[string]*exec.Cmd{}
)

// Запускает этот же exe с флагом (-console или -procs): он создаёт своё окно консоли.
func openConsole(mode string) {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	if consoleCmds[mode] != nil {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	args := append(append([]string{}, os.Args[1:]...), mode)
	cmd := exec.Command(exe, args...)
	if err := cmd.Start(); err != nil {
		return
	}
	consoleCmds[mode] = cmd
	go func() {
		cmd.Wait()
		consoleMu.Lock()
		delete(consoleCmds, mode)
		consoleMu.Unlock()
	}()
}

func killConsole() {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	for _, c := range consoleCmds {
		c.Process.Kill()
	}
}

func consoleCell(label string, p *path, isDirect bool) string {
	if !p.ok {
		return fmt.Sprintf("%s%s: НЕТ ОТВЕТА %s%s", red, label, time.Since(p.failStart).Round(time.Second), reset)
	}
	s := p.stats()
	col := green
	if (isDirect && p.rtt > time.Duration(*slowPing)*time.Millisecond) || s.loss > *slowLoss {
		col = yellow
	}
	return fmt.Sprintf("%s%s: %3d мс · потери %.0f%% · джиттер %.0f мс%s", col, label, p.rtt.Milliseconds(), s.loss, s.jitter, reset)
}

func failDetail(label string, p *path) string {
	s := p.stats()
	last := "ещё не было"
	if !p.lastOK.IsZero() {
		last = fmt.Sprintf("%s (%s назад)", p.lastOK.Format("15:04:05"), time.Since(p.lastOK).Round(time.Second))
	}
	return fmt.Sprintf("%s   >> %s: %s. Проверок подряд без ответа: %d. Потери за окно: %.0f%%. Последний успех: %s%s",
		red, label, p.reason, p.consecFail, s.loss, last, reset)
}

func printLine() {
	mu.Lock()
	defer mu.Unlock()

	var sb strings.Builder
	sb.WriteString(time.Now().Format("15:04:05") + "  ")
	sb.WriteString(consoleCell(direct.name, direct, true))
	var details []string
	if !direct.ok && (direct.consecFail == 1 || direct.consecFail%10 == 0) {
		details = append(details, failDetail(direct.name, direct))
	}
	if viaProxy != nil {
		sb.WriteString("  │  " + consoleCell(viaProxy.name, viaProxy, false))
		if !viaProxy.ok && (viaProxy.consecFail == 1 || viaProxy.consecFail%10 == 0) {
			details = append(details, failDetail(viaProxy.name, viaProxy))
		}
	}
	var sp []string
	if direct.hasSpeed {
		sp = append(sp, fmt.Sprintf("напрямую %.1f/%.1f", direct.down, direct.up))
	}
	if viaProxy != nil && viaProxy.hasSpeed {
		sp = append(sp, fmt.Sprintf("через прокси %.1f/%.1f", viaProxy.down, viaProxy.up))
	}
	if len(sp) > 0 {
		sb.WriteString("\n                      скорость (загрузка/отдача): " + strings.Join(sp, " · ") + " Мбит/с")
	}
	fmt.Println(sb.String())
	for _, d := range details {
		fmt.Println(d)
	}
}

func tailLog(n int) []string {
	if logPath == "" {
		return nil
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

func runConsole() {
	g := func(f string, a ...any) { fmt.Printf(gray+f+reset+"\n", a...) }
	g("Это окно можно закрыть в любой момент — значок в трее продолжит работать.")
	g("")
	g("Что проверяется (каждые %s):", *interval)
	g("  %s", direct.name)
	g("      TCP-подключение к серверу мимо прокси. Пинг = время установки соединения.")
	g("      Так идёт трафик без прокси (как правило, и голос Discord — он работает по UDP).")
	if viaProxy != nil {
		g("  %s", viaProxy.name)
		g("      HTTP-запрос к cp.cloudflare.com через ваш прокси. Пинг = время ответа.")
		g("      Так идёт трафик браузера через Xray.")
	} else {
		g("  Прокси не найден — проверяется только прямой путь (укажи его флагом -proxy).")
	}
	g("  Потери — доля неудачных проверок из последних 60. Джиттер — средний разброс пинга.")
	g("  Скорость — загрузка и отправка данных на speed.cloudflare.com, раз в %s.", *every)
	if lines := tailLog(15); len(lines) > 0 {
		fmt.Printf("\n%sПоследние события из лога:%s\n", gray, reset)
		for _, l := range lines {
			fmt.Println(gray + l + reset)
		}
	}
	fmt.Println()
	monitor()
}

func openLog() (*os.File, string) {
	var cands []string
	if exe, err := os.Executable(); err == nil {
		cands = append(cands, filepath.Join(filepath.Dir(exe), "netmon.log"))
	}
	cands = append(cands, filepath.Join(os.TempDir(), "netmon.log"))
	for _, c := range cands {
		if f, err := os.OpenFile(c, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			return f, c
		}
	}
	return nil, ""
}

func main() {
	flag.Parse()

	if *consoleFlag || *procsFlag {
		attachConsole()
	}
	if *consoleFlag {
		fmt.Printf("%sNetmon — подробный вывод%s\n", cyan, reset)
	}
	logFile, logPath = openLog()
	if (*consoleFlag || *procsFlag) && logFile != nil { // окно консоли лог только читает, пишет трей
		logFile.Close()
		logFile = nil
	}

	pu := ""
	switch *proxyFlag {
	case "auto":
		pu = systemProxy()
	case "none", "":
	default:
		pu = *proxyFlag
		if !strings.Contains(pu, "://") {
			pu = "http://" + pu
		}
	}
	if pu != "" {
		if u, err := url.Parse(pu); err == nil && u.Host != "" {
			proxyURL = u
		}
	}

	direct = &path{name: "Напрямую (" + *host + ")", client: &http.Client{Transport: &http.Transport{Proxy: nil}}}
	if proxyURL != nil {
		viaProxy = &path{name: "Через прокси (" + proxyURL.Host + ")", client: &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}}
		probeClient = &http.Client{Transport: &http.Transport{
			Proxy:             http.ProxyURL(proxyURL),
			DisableKeepAlives: true, // каждый раз проверяем весь путь заново
		}}
		logf("запуск, прокси: %s", proxyURL)
	} else {
		logf("запуск, прокси не найден")
	}

	if *procsFlag {
		runProcs()
		return
	}
	if *consoleFlag {
		runConsole()
		return
	}
	systray.Run(onReady, func() {})
}
