package main

import (
	"bytes"
	"compress/zlib"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

// Nazwa dokumentu widoczna w bibliotece na tablecie.
const docVisibleName = "reMarkable Live – adres"

// urlState trzyma aktualny publiczny adres tunelu (z tunnel-url.txt).
type urlState struct {
	mu  sync.RWMutex
	url string
}

func (s *urlState) get() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.url
}

// watchURLFile co kilka sekund czyta plik z adresem; gdy adres się zmieni,
// aktualizuje stan i (opcjonalnie) publikuje dokument PDF w bibliotece xochitl.
func watchURLFile(path string, publishDoc bool, xochitlDir string, st *urlState) {
	// Adres już opublikowany w dokumencie jest zapamiętany obok pliku z adresem,
	// żeby restart proxy nie publikował go (i nie restartował xochitl) ponownie.
	publishedFile := path + ".published"
	published := ""
	if b, err := os.ReadFile(publishedFile); err == nil {
		published = strings.TrimSpace(string(b))
	}
	var last string
	for {
		b, err := os.ReadFile(path)
		cur := ""
		if err == nil {
			cur = strings.TrimSpace(string(b))
		}
		if cur != last {
			last = cur
			st.mu.Lock()
			st.url = cur
			st.mu.Unlock()
			if cur != "" {
				log.Printf("adres tunelu: %s", cur)
				if publishDoc && cur != published {
					if err := publishURLDocument(xochitlDir, cur); err != nil {
						log.Printf("nie udało się opublikować dokumentu z adresem: %v", err)
					} else {
						published = cur
						_ = os.WriteFile(publishedFile, []byte(cur+"\n"), 0o644)
					}
				}
			}
		}
		time.Sleep(5 * time.Second)
	}
}

// publishURLDocument zapisuje w bibliotece xochitl dokument PDF (adres + kod QR)
// i restartuje xochitl oraz streamer, żeby tablet go zobaczył. UUID dokumentu
// jest stały (zapisany obok pliku z adresem), więc kolejne adresy nadpisują
// ten sam dokument zamiast mnożyć kopie.
func publishURLDocument(xochitlDir, url string) error {
	if _, err := os.Stat(xochitlDir); err != nil {
		return fmt.Errorf("brak katalogu xochitl %s: %w", xochitlDir, err)
	}
	id, err := stableDocID(filepath.Join(filepath.Dir(xochitlDir), "rmshare-doc-id"))
	if err != nil {
		return err
	}
	pdf, err := buildURLPDF(url)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	meta := map[string]any{
		"deleted": false, "lastModified": fmt.Sprint(now), "lastOpened": "", "lastOpenedPage": 0,
		"metadatamodified": false, "modified": false, "parent": "", "pinned": true,
		"synced": false, "type": "DocumentType", "version": 1, "visibleName": docVisibleName,
	}
	content := map[string]any{
		"coverPageNumber": 0, "documentMetadata": map[string]any{}, "dummyDocument": false,
		"extraMetadata": map[string]any{}, "fileType": "pdf", "fontName": "", "formatVersion": 1,
		"lineHeight": -1, "margins": 125, "orientation": "portrait", "pageCount": 1,
		"pageTags": []any{}, "pages": []any{}, "sizeInBytes": fmt.Sprint(len(pdf)), "tags": []any{},
		"textAlignment": "justify", "textScale": 1,
		"transform": map[string]any{"m11": 1, "m12": 0, "m13": 0, "m21": 0, "m22": 1, "m23": 0, "m31": 0, "m32": 0, "m33": 1},
	}
	base := filepath.Join(xochitlDir, id)
	if err := os.WriteFile(base+".pdf", pdf, 0o644); err != nil {
		return err
	}
	if err := writeJSON(base+".metadata", meta); err != nil {
		return err
	}
	if err := writeJSON(base+".content", content); err != nil {
		return err
	}
	// Katalog na adnotacje; xochitl lubi, gdy istnieje.
	_ = os.MkdirAll(base, 0o755)

	log.Printf("dokument %q zapisany (%s), restartuję xochitl", docVisibleName, id)
	// goMarkableStream znajduje bufor ekranu przy starcie, więc po restarcie
	// xochitl musi wystartować ponownie — ale dopiero gdy xochitl zdąży
	// zmapować ekran (inaczej kończy się błędem "no mapping found").
	if out, err := exec.Command("systemctl", "restart", "xochitl.service").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl restart xochitl: %v: %s", err, out)
	}
	time.Sleep(8 * time.Second)
	if out, err := exec.Command("systemctl", "restart", "rmshare-stream.service").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl restart rmshare-stream: %v: %s", err, out)
	}
	return nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "    ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// stableDocID zwraca UUID zapisany w pliku, tworząc go przy pierwszym użyciu.
func stableDocID(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) == 36 {
		return strings.TrimSpace(string(b)), nil
	}
	b := make([]byte, 16)
	if _, err := randRead(b); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	id := fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	return id, os.WriteFile(path, []byte(id+"\n"), 0o644)
}

// buildURLPDF tworzy jednostronicowy PDF (A4-ish, proporcje ekranu rM) z tytułem,
// adresem i kodem QR. Bez zewnętrznych bibliotek: tekst Helvetica + obraz
// w skali szarości skompresowany Flate.
func buildURLPDF(url string) ([]byte, error) {
	qr, err := qrcode.New(url, qrcode.Medium)
	if err != nil {
		return nil, err
	}
	bm := qr.Bitmap() // zawiera już margines ciszy
	n := len(bm)
	raster := make([]byte, n*n)
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if bm[y][x] {
				raster[y*n+x] = 0x00
			} else {
				raster[y*n+x] = 0xff
			}
		}
	}
	var zb bytes.Buffer
	zw := zlib.NewWriter(&zb)
	zw.Write(raster)
	zw.Close()

	// Strona 445x594 pt (proporcje 3:4 jak ekran reMarkable).
	const pw, ph = 445.0, 594.0
	qrSize := 300.0
	qrX := (pw - qrSize) / 2
	qrY := 150.0
	content := fmt.Sprintf(
		"BT /F1 22 Tf 40 540 Td (reMarkable Live) Tj ET\n"+
			"BT /F2 11 Tf 40 515 Td (Zeskanuj kod lub wpisz adres w przegladarce, potem podaj haslo.) Tj ET\n"+
			"BT /F2 9 Tf 40 500 Td (Wygenerowano: %s) Tj ET\n"+
			"q %.0f 0 0 %.0f %.0f %.0f cm /Im1 Do Q\n"+
			"BT /F1 %d Tf %.0f 110 Td (%s) Tj ET\n",
		time.Now().Format("2006-01-02 15:04"),
		qrSize, qrSize, qrX, qrY,
		urlFontSize(url), 40.0, pdfEscape(url))

	var b bytes.Buffer
	offsets := []int{}
	obj := func(s string) {
		offsets = append(offsets, b.Len())
		b.WriteString(s)
	}
	b.WriteString("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
	obj("1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n")
	obj("2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n")
	obj(fmt.Sprintf("3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %.0f %.0f] /Contents 4 0 R "+
		"/Resources << /Font << /F1 5 0 R /F2 6 0 R >> /XObject << /Im1 7 0 R >> >> >>\nendobj\n", pw, ph))
	obj(fmt.Sprintf("4 0 obj\n<< /Length %d >>\nstream\n%s\nendstream\nendobj\n", len(content), content))
	obj("5 0 obj\n<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica-Bold >>\nendobj\n")
	obj("6 0 obj\n<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>\nendobj\n")
	offsets = append(offsets, b.Len())
	fmt.Fprintf(&b, "7 0 obj\n<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceGray "+
		"/BitsPerComponent 8 /Interpolate false /Filter /FlateDecode /Length %d >>\nstream\n", n, n, zb.Len())
	b.Write(zb.Bytes())
	b.WriteString("\nendstream\nendobj\n")
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(offsets)+1)
	for _, o := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", o)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets)+1, xref)
	return b.Bytes(), nil
}

// urlFontSize dobiera rozmiar czcionki tak, by adres zmieścił się na stronie.
func urlFontSize(url string) int {
	// Helvetica-Bold ~0.6 em na znak; szerokość tekstu 365 pt.
	size := int(365.0 / (0.6 * float64(len(url))))
	if size > 18 {
		size = 18
	}
	if size < 7 {
		size = 7
	}
	return size
}

func pdfEscape(s string) string {
	r := strings.NewReplacer("\\", "\\\\", "(", "\\(", ")", "\\)")
	return r.Replace(s)
}

// urlPageHTML to strona /url: aktualny adres + QR.
const urlPageHTML = `<!doctype html><html lang="pl"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1"><title>reMarkable Live – adres</title>
<style>body{font-family:-apple-system,system-ui,sans-serif;display:grid;place-items:center;min-height:100vh;margin:0;background:#f3f1ec;color:#1d1d1b}
main{text-align:center;padding:24px}a{word-break:break-all;font-size:1.1rem}img{width:min(70vw,320px);image-rendering:pixelated}
p.sub{opacity:.65}</style></head><body><main>
<h1>Aktualny adres</h1>%s
<p class="sub"><a href="/">← wróć do podglądu</a> · <a href="/url?format=txt">tekst</a></p></main></body></html>`

func handleURL(st *urlState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		u := st.get()
		if r.URL.Query().Get("format") == "txt" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintln(w, u)
			return
		}
		body := "<p>Adres nie jest jeszcze znany (tunel się uruchamia).</p>"
		if u != "" {
			body = fmt.Sprintf(`<p><a href="%s">%s</a></p><img src="/url/qr.png" alt="QR">`, u, u)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, urlPageHTML, body)
	}
}

func handleURLQR(st *urlState) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := st.get()
		if u == "" {
			http.NotFound(w, r)
			return
		}
		png, err := qrcode.Encode(u, qrcode.Medium, 320)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(png)
	}
}

func randRead(b []byte) (int, error) { return rand.Read(b) }
