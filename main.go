package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	webview2 "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
)

//go:embed ui.html
var uiHTML string

type Config struct {
	APIKeys    []string `json:"api_keys"`
	APIKey     string   `json:"api_key,omitempty"` // format lama, dipindah ke APIKeys
	Model      string   `json:"model"`
	Dim        int      `json:"dim"`
	Folder     string   `json:"folder"`
	Exclude    []string `json:"exclude"`
	ExcludeSet bool     `json:"exclude_set"` // defaults already applied once
}

// Folders under Downloads that held datasets, game data, source code or extracted installers.
var defaultExcludes = []string{
	"ABDM", "ES-DE_3.3.0-x64_Portable", "2", "ARA", "PJK05", "PED-07", "PED08", "Pos 7",
	"POSTEST 07(2)", "POSTEST PJK-04", "POSTESTTTT AAAAA", "Compressed", "KyoUraAmiibo",
	"Amazon.com_ Asus Rog_files", "Quiz - KE - 2022 Oktober_files", "clean-code-python-latihan",
	"SCP-DS-Driver-Package-1.2.0.160", "kafka", "kafka_2.13-4.0.0", "kafka-project", "ROMS", "PS2_BIOS",
	"idm", "IDM.6.42.b.58.kuyhAa", "[BAGAS31] - IDM_6.42_Build_15", "WinRAR 6.24.kuyhAa",
	"CCleaner.v6.30.11385.Pro.Bu.Tech.kuyhAa", "Doload.org_InstruCalc_9.0.0", "InstruCalc 9.0.0",
	"UltimaScraper-master", "ecommerce-scraper", "testing_python_course", "dsa-chapter-01",
	"airflow_starter_pack", "tolldata", "groupy-extension-2.3-release-fix", "groupy-guard-1.0.4-release-fix",
	"vpngate-client-2025.05.12-build-9807.161763", "spacesniffer_2_0_5_18_x64 (1)", "Untitled-1_Images",
}

func (c *Config) addExclude(p string) bool {
	p = filepath.Clean(strings.TrimSpace(p))
	if p == "." || p == "" {
		return false
	}
	for _, e := range c.Exclude {
		if strings.EqualFold(e, p) {
			return false
		}
	}
	c.Exclude = append(c.Exclude, p)
	return true
}

func (c *Config) addKey(k string) bool {
	k = strings.TrimSpace(k)
	if k == "" {
		return false
	}
	for _, e := range c.APIKeys {
		if e == k {
			return false
		}
	}
	c.APIKeys = append(c.APIKeys, k)
	return true
}

func maskKey(k string) string {
	if len(k) <= 10 {
		return strings.Repeat("•", len(k))
	}
	return k[:4] + strings.Repeat("•", 8) + k[len(k)-4:]
}

type App struct {
	dir   string
	mu    sync.Mutex
	cfg   Config
	store *Store
	fake  bool
}

func appDir() string {
	base, err := os.UserConfigDir()
	if err != nil {
		base = "."
	}
	d := filepath.Join(base, "Sherlock")
	os.MkdirAll(d, 0o755)
	return d
}

func (a *App) loadConfig() {
	home, _ := os.UserHomeDir()
	a.cfg = Config{Model: "gemini-embedding-2", Dim: 768, Folder: filepath.Join(home, "Downloads")}
	if data, err := os.ReadFile(filepath.Join(a.dir, "config.json")); err == nil {
		json.Unmarshal(data, &a.cfg)
	}
	if a.cfg.APIKey != "" {
		a.cfg.addKey(a.cfg.APIKey)
		a.cfg.APIKey = ""
		a.saveConfig()
	}
	if len(a.cfg.APIKeys) == 0 {
		a.cfg.addKey(os.Getenv("GEMINI_API_KEY"))
	}
	if !a.cfg.ExcludeSet {
		dl := filepath.Join(home, "Downloads")
		for _, d := range defaultExcludes {
			if st, err := os.Stat(filepath.Join(dl, d)); err == nil && st.IsDir() {
				a.cfg.addExclude(filepath.Join(dl, d))
			}
		}
		a.cfg.ExcludeSet = true
		a.saveConfig()
	}
}

func (a *App) saveConfig() error {
	data, _ := json.MarshalIndent(a.cfg, "", "  ")
	return os.WriteFile(filepath.Join(a.dir, "config.json"), data, 0o600)
}

func (a *App) client() (*Client, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.cfg.APIKeys) == 0 && !a.fake {
		return nil, fmt.Errorf("API key Gemini belum diisi. Buka Pengaturan.")
	}
	return NewClient(a.cfg.APIKeys, a.cfg.Model, a.cfg.Dim, a.fake), nil
}

func main() {
	indexDir := flag.String("index", "", "index folder lalu keluar (mode CLI)")
	query := flag.String("search", "", "cari lalu keluar (mode CLI)")
	browser := flag.Bool("browser", false, "buka di browser, bukan jendela")
	flag.Parse()

	a := &App{dir: appDir(), fake: os.Getenv("SHERLOCK_FAKE") == "1"}
	if d := os.Getenv("SHERLOCK_DIR"); d != "" {
		a.dir = d
		os.MkdirAll(d, 0o755)
	}
	a.loadConfig()
	a.store = LoadStore(filepath.Join(a.dir, "index.gob"))
	startOffice(filepath.Join(a.dir, "tmp"))
	a.store.SetExcluded(a.cfg.Exclude)

	if *indexDir != "" || *query != "" {
		runCLI(a, *indexDir, *query)
		return
	}

	token := randHex(16)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fatalBox(err.Error())
		return
	}
	go http.Serve(ln, a.routes(token))
	url := fmt.Sprintf("http://%s/?t=%s", ln.Addr().String(), token)
	if os.Getenv("SHERLOCK_SERVE") == "1" { // dev: hanya server, tanpa jendela
		fmt.Println(url)
		select {}
	}

	if !*browser {
		w := webview2.NewWithOptions(webview2.WebViewOptions{
			AutoFocus: true,
			DataPath:  filepath.Join(a.dir, "webview"),
			WindowOptions: webview2.WindowOptions{
				Title: "Sherlock", Width: 1100, Height: 760, Center: true, IconId: 1,
			},
		})
		if w != nil {
			defer w.Destroy()
			w.Navigate(url)
			// Minimize/close hide Sherlock to the tray; Alt+Space or the tray icon bring it back
			// with the search box focused. "Keluar" in the tray menu really quits.
			t := startTray(uintptr(w.Window()),
				func() { w.Dispatch(func() { w.Eval("window.sherlockFocus && sherlockFocus()") }) },
				func() { w.Dispatch(w.Terminate) })
			if !t.HotkeyOK {
				log.Print("Alt+Space tidak bisa didaftarkan (dipakai aplikasi lain?)")
			}
			w.Run()
			t.Remove()
			a.store.Cancel()
			stopOffice()
			time.Sleep(300 * time.Millisecond)
			return
		}
	}
	shellOpen(url)
	select {}
}

func runCLI(a *App, dir, q string) {
	c, err := a.client()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	if dir != "" {
		if err := a.store.StartIndex(dir, c); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		for a.store.GetStatus().Running {
			time.Sleep(500 * time.Millisecond)
			st := a.store.GetStatus()
			fmt.Printf("\r%d/%d gagal=%d %-40.40s", st.Done, st.Total, st.Failed, st.Current)
		}
		fmt.Println("\n" + a.store.GetStatus().Message)
	}
	if q != "" {
		qv, err := c.EmbedQuery(context.Background(), q)
		if err != nil {
			fmt.Println(friendlyErr(err))
			os.Exit(1)
		}
		for _, r := range a.store.Search(qv, q, "", 10) {
			fmt.Printf("%.3f  [%s/%s]  %s\n", r.Score, r.Kind, r.Mode, r.Path)
		}
	}
}

func (a *App) routes(token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" || r.URL.Query().Get("t") != token {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(uiHTML))
	})
	api := func(path string, h func(r *http.Request) (any, error)) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Token") != token {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			v, err := h(r)
			w.Header().Set("Content-Type", "application/json")
			if err != nil {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			json.NewEncoder(w).Encode(v)
		})
	}
	decode := func(r *http.Request, v any) error { return json.NewDecoder(r.Body).Decode(v) }

	api("/api/config", func(r *http.Request) (any, error) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if r.Method == "POST" {
			var in struct {
				AddKey        string `json:"add_key"`
				RemoveKey     *int   `json:"remove_key"`
				Model         string `json:"model"`
				Dim           int    `json:"dim"`
				Folder        string `json:"folder"`
				AddExclude    string `json:"add_exclude"`
				RemoveExclude *int   `json:"remove_exclude"`
			}
			if err := decode(r, &in); err != nil {
				return nil, err
			}
			if in.AddExclude != "" || in.RemoveExclude != nil {
				if in.AddExclude != "" {
					a.cfg.addExclude(in.AddExclude)
				}
				if i := in.RemoveExclude; i != nil && *i >= 0 && *i < len(a.cfg.Exclude) {
					a.cfg.Exclude = append(a.cfg.Exclude[:*i], a.cfg.Exclude[*i+1:]...)
				}
				a.store.SetExcluded(a.cfg.Exclude)
			}
			// Each pasted line or comma-separated value counts as one key.
			for _, k := range strings.FieldsFunc(in.AddKey, func(r rune) bool { return r == '\n' || r == ',' || r == ' ' }) {
				a.cfg.addKey(k)
			}
			if i := in.RemoveKey; i != nil && *i >= 0 && *i < len(a.cfg.APIKeys) {
				a.cfg.APIKeys = append(a.cfg.APIKeys[:*i], a.cfg.APIKeys[*i+1:]...)
			}
			if in.Model != "" {
				a.cfg.Model = strings.TrimSpace(in.Model)
			}
			if in.Dim == 768 || in.Dim == 1536 || in.Dim == 3072 {
				a.cfg.Dim = in.Dim
			}
			if in.Folder != "" {
				a.cfg.Folder = in.Folder
			}
			if err := a.saveConfig(); err != nil {
				return nil, err
			}
		}
		masked := make([]string, len(a.cfg.APIKeys))
		for i, k := range a.cfg.APIKeys {
			masked[i] = maskKey(k)
		}
		return map[string]any{"has_key": len(a.cfg.APIKeys) > 0 || a.fake, "keys": masked, "model": a.cfg.Model,
			"dim": a.cfg.Dim, "folder": a.cfg.Folder, "count": a.store.Count(), "fake": a.fake,
			"exclude": append([]string{}, a.cfg.Exclude...)}, nil
	})
	// Thumbnails: <img> tags can't send headers, so the token rides in the query string.
	mux.HandleFunc("/api/thumb", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("p")
		if r.URL.Query().Get("t") != token || !a.store.Has(p) || !hasPreview(p) {
			http.NotFound(w, r)
			return
		}
		data, err := a.thumb(p)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "max-age=86400")
		w.Write(data)
	})
	api("/api/index", func(r *http.Request) (any, error) {
		var in struct{ Folder string }
		decode(r, &in)
		if st, err := os.Stat(in.Folder); err != nil || !st.IsDir() {
			return nil, fmt.Errorf("folder tidak ditemukan: %s", in.Folder)
		}
		c, err := a.client()
		if err != nil {
			return nil, err
		}
		a.mu.Lock()
		a.cfg.Folder = in.Folder
		a.saveConfig()
		a.mu.Unlock()
		return "ok", a.store.StartIndex(in.Folder, c)
	})
	api("/api/cancel", func(r *http.Request) (any, error) { a.store.Cancel(); return "ok", nil })
	api("/api/status", func(r *http.Request) (any, error) {
		return map[string]any{"status": a.store.GetStatus(), "count": a.store.Count()}, nil
	})
	api("/api/search", func(r *http.Request) (any, error) {
		var in struct{ Q, Kind string }
		decode(r, &in)
		if strings.TrimSpace(in.Q) == "" {
			return []Result{}, nil
		}
		c, err := a.client()
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		qv, err := c.EmbedQuery(ctx, in.Q)
		if err != nil {
			return nil, fmt.Errorf("%s", friendlyErr(err))
		}
		return a.store.Search(qv, in.Q, in.Kind, 60), nil
	})
	api("/api/open", func(r *http.Request) (any, error) {
		var in struct{ Path string }
		decode(r, &in)
		if !a.store.Has(in.Path) {
			return nil, fmt.Errorf("file tidak ada di index")
		}
		return "ok", shellOpen(in.Path)
	})
	api("/api/reveal", func(r *http.Request) (any, error) {
		var in struct{ Path string }
		decode(r, &in)
		if !a.store.Has(in.Path) {
			return nil, fmt.Errorf("file tidak ada di index")
		}
		cmd := exec.Command("explorer.exe")
		cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe /select,"` + in.Path + `"`}
		return "ok", cmd.Start()
	})
	api("/api/pick", func(r *http.Request) (any, error) {
		ps := `Add-Type -AssemblyName System.Windows.Forms; $d = New-Object System.Windows.Forms.FolderBrowserDialog; ` +
			`$d.Description = 'Pilih folder untuk di-index'; if ($d.ShowDialog() -eq 'OK') { [Console]::OutputEncoding=[Text.Encoding]::UTF8; $d.SelectedPath }`
		cmd := exec.Command("powershell.exe", "-NoProfile", "-STA", "-Command", ps)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		out, err := cmd.Output()
		if err != nil {
			return nil, err
		}
		return strings.TrimSpace(string(out)), nil
	})
	return mux
}

func shellOpen(target string) error {
	return windows.ShellExecute(0, windows.StringToUTF16Ptr("open"), windows.StringToUTF16Ptr(target), nil, nil, windows.SW_SHOWNORMAL)
}

func fatalBox(msg string) {
	windows.MessageBox(0, windows.StringToUTF16Ptr(msg), windows.StringToUTF16Ptr("Sherlock"), windows.MB_ICONERROR)
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
