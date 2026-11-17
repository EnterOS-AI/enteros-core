// Command pushsink is a minimal, push-only OCI distribution endpoint for the
// ephemeral-CP happy path (tests/e2e/ephemeral_cp_happy_path.sh,
// stage_tenant_pin_image). It exists so the harness can `docker push` the PR's
// tenant image once and have the daemon record a repository digest for it; it is
// never a general registry.
//
// It is standard-library only on purpose: the first cut built
// distribution/distribution in CI, whose ~150-module dependency graph made a
// required gate depend on the Go module proxy and checksum database being up
// (run 851124 died on a sumdb 5xx before staging anything).
//
// Implemented: GET /v2/; blob HEAD/GET; upload POST (a cross-repo mount request
// starts a normal upload), PATCH, PUT ?digest= (verified); manifest PUT/HEAD/GET
// (digest = sha256 of the exact bytes). No auth, no deletes, no listing. Blobs
// and manifests are content-addressed files under -root.
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

var (
	digestRe = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	nameRe   = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*$`)
	refRe    = regexp.MustCompile(`^(?:[A-Za-z0-9_][A-Za-z0-9._-]{0,127}|sha256:[a-f0-9]{64})$`)
)

type sink struct {
	root string
	mu   sync.Mutex
	// manifests maps "<name>:<reference>" to the manifest digest and media type.
	manifests map[string][2]string
}

func (s *sink) blobPath(d string) string { return filepath.Join(s.root, "blobs", d[len("sha256:"):]) }
func (s *sink) uploadPath(id string) string {
	return filepath.Join(s.root, "uploads", id)
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func fileDigest(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), n, nil
}

func (s *sink) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	p := r.URL.Path
	if p == "/v2/" || p == "/v2" {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "{}")
		return
	}
	if !strings.HasPrefix(p, "/v2/") {
		http.NotFound(w, r)
		return
	}
	rest := strings.TrimPrefix(p, "/v2/")
	switch {
	case strings.Contains(rest, "/blobs/uploads"):
		i := strings.Index(rest, "/blobs/uploads")
		s.upload(w, r, rest[:i], strings.Trim(rest[i+len("/blobs/uploads"):], "/"))
	case strings.Contains(rest, "/blobs/"):
		i := strings.LastIndex(rest, "/blobs/")
		s.blob(w, r, rest[:i], rest[i+len("/blobs/"):])
	case strings.Contains(rest, "/manifests/"):
		i := strings.LastIndex(rest, "/manifests/")
		s.manifest(w, r, rest[:i], rest[i+len("/manifests/"):])
	default:
		http.NotFound(w, r)
	}
}

func (s *sink) blob(w http.ResponseWriter, r *http.Request, name, d string) {
	if !nameRe.MatchString(name) || !digestRe.MatchString(d) {
		http.Error(w, "bad name or digest", http.StatusBadRequest)
		return
	}
	if r.Method != http.MethodHead && r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	fi, err := os.Stat(s.blobPath(d))
	if err != nil {
		http.Error(w, "blob unknown", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(fi.Size()))
	w.Header().Set("Docker-Content-Digest", d)
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	http.ServeFile(w, r, s.blobPath(d))
}

func (s *sink) upload(w http.ResponseWriter, r *http.Request, name, id string) {
	if !nameRe.MatchString(name) {
		http.Error(w, "bad name", http.StatusBadRequest)
		return
	}
	loc := func(id string) string { return "/v2/" + name + "/blobs/uploads/" + id }
	switch {
	case r.Method == http.MethodPost && id == "":
		// A ?mount= request is answered by starting an ordinary upload (202),
		// which the client treats as "not mounted".
		id = newID()
		f, err := os.Create(s.uploadPath(id))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		f.Close()
		w.Header().Set("Location", loc(id))
		w.Header().Set("Docker-Upload-UUID", id)
		w.Header().Set("Range", "0-0")
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusAccepted)
	case (r.Method == http.MethodPatch || r.Method == http.MethodPut) && id != "":
		if strings.ContainsAny(id, "/.") {
			http.Error(w, "bad upload id", http.StatusBadRequest)
			return
		}
		f, err := os.OpenFile(s.uploadPath(id), os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			http.Error(w, "upload unknown", http.StatusNotFound)
			return
		}
		_, err = io.Copy(f, r.Body)
		cerr := f.Close()
		if err != nil || cerr != nil {
			http.Error(w, "write failed", http.StatusInternalServerError)
			return
		}
		fi, err := os.Stat(s.uploadPath(id))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if r.Method == http.MethodPatch {
			w.Header().Set("Location", loc(id))
			w.Header().Set("Docker-Upload-UUID", id)
			w.Header().Set("Range", fmt.Sprintf("0-%d", max(fi.Size()-1, 0)))
			w.Header().Set("Content-Length", "0")
			w.WriteHeader(http.StatusAccepted)
			return
		}
		want := r.URL.Query().Get("digest")
		if !digestRe.MatchString(want) {
			http.Error(w, "digest required", http.StatusBadRequest)
			return
		}
		got, _, err := fileDigest(s.uploadPath(id))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if got != want {
			os.Remove(s.uploadPath(id))
			http.Error(w, "digest mismatch", http.StatusBadRequest)
			return
		}
		if err := os.Rename(s.uploadPath(id), s.blobPath(want)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Location", "/v2/"+name+"/blobs/"+want)
		w.Header().Set("Docker-Content-Digest", want)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusCreated)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *sink) manifest(w http.ResponseWriter, r *http.Request, name, ref string) {
	if !nameRe.MatchString(name) || !refRe.MatchString(ref) {
		http.Error(w, "bad name or reference", http.StatusBadRequest)
		return
	}
	key := name + ":" + ref
	switch r.Method {
	case http.MethodPut:
		body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		sum := sha256.Sum256(body)
		d := "sha256:" + hex.EncodeToString(sum[:])
		if strings.HasPrefix(ref, "sha256:") && ref != d {
			http.Error(w, "digest mismatch", http.StatusBadRequest)
			return
		}
		if err := os.WriteFile(s.blobPath(d), body, 0o600); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		mt := r.Header.Get("Content-Type")
		s.mu.Lock()
		s.manifests[key] = [2]string{d, mt}
		s.manifests[name+":"+d] = [2]string{d, mt}
		s.mu.Unlock()
		log.Printf("pushsink: manifest %s:%s = %s", name, ref, d)
		w.Header().Set("Location", "/v2/"+name+"/manifests/"+d)
		w.Header().Set("Docker-Content-Digest", d)
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusCreated)
	case http.MethodHead, http.MethodGet:
		s.mu.Lock()
		m, ok := s.manifests[key]
		s.mu.Unlock()
		if !ok {
			http.Error(w, "manifest unknown", http.StatusNotFound)
			return
		}
		body, err := os.ReadFile(s.blobPath(m[0]))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if m[1] != "" {
			w.Header().Set("Content-Type", m[1])
		}
		w.Header().Set("Docker-Content-Digest", m[0])
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(body)
		}
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func main() {
	addr := flag.String("addr", "127.0.0.1:80", "listen address")
	root := flag.String("root", "", "storage directory (created if missing)")
	flag.Parse()
	if *root == "" {
		log.Fatal("pushsink: -root is required")
	}
	for _, d := range []string{"blobs", "uploads"} {
		if err := os.MkdirAll(filepath.Join(*root, d), 0o700); err != nil {
			log.Fatal(err)
		}
	}
	s := &sink{root: *root, manifests: map[string][2]string{}}
	log.Printf("pushsink: listening on %s, root %s", *addr, *root)
	log.Fatal(http.ListenAndServe(*addr, s))
}
