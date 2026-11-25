package server

import (
	_ "embed"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// One-line installers and the binaries they download. seatimed serves
// /install.sh, /install.ps1 and /dl/{file} from the directory SEATIME_DL_DIR
// (filled by scripts/release.sh: seatime-{os}-{arch}[.exe], SHA256SUMS and
// the signed RELEASE manifest clients update from), or redirects /dl/{file}
// to SEATIME_DL_UPSTREAM/{file} when that is set.

//go:embed scripts/install.sh
var installSH string

//go:embed scripts/install.ps1
var installPS1 string

// Only these names are ever served from the download dir.
var dlName = regexp.MustCompile(`^(SHA256SUMS|RELEASE|seatime-(windows|darwin|linux)-(amd64|386|arm64)(\.exe)?)$`)

// A public URL is spliced into shell/PowerShell source, so it must be boring.
var safeURL = regexp.MustCompile(`^https?://[A-Za-z0-9._:\[\]-]+(/[A-Za-z0-9._~/-]*)?$`)

func (s *Server) handleInstallScript(src, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		base := strings.TrimRight(s.cfg.BaseURL, "/")
		if !safeURL.MatchString(base) {
			http.Error(w, "SEATIME_BASE_URL is not a plain http(s) URL; refusing to template an installer with it", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write([]byte(strings.ReplaceAll(src, "@@SERVER@@", base)))
	}
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if s.cfg.DLUpstream != "" && dlName.MatchString(name) {
		w.Header().Set("Cache-Control", "no-cache")
		http.Redirect(w, r, s.cfg.DLUpstream+"/"+name, http.StatusFound)
		return
	}
	if s.cfg.DLDir == "" || !dlName.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(s.cfg.DLDir, name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	// ~10 MB binaries on slow links outlive the server-wide write timeout.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(15 * time.Minute))
	ct := "application/octet-stream"
	if name == "SHA256SUMS" || name == "RELEASE" {
		ct = "text/plain; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-cache") // revalidate: a new release replaces files in place
	http.ServeContent(w, r, name, fi.ModTime(), f)
}
