// vm-runtime-helper is a disabled-by-default local privileged KVM controller.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/vmruntime"
)

func main() {
	enabled := flag.Bool("enabled", false, "explicitly enable the privileged helper")
	root := flag.String("root", "/var/lib/sub2-vm", "private persistent instance root")
	socket := flag.String("socket", "/run/sub2-vm/helper.sock", "private local Unix socket")
	config := flag.String("templates", "", "operator-owned fixed template JSON")
	allowTCG := flag.Bool("allow-tcg", false, "explicitly allow software emulation for testing")
	serviceGID := flag.Int("service-gid", 0, "optional dedicated application service group")
	serviceUID := flag.Int("service-uid", 0, "application UID owning the automatically prepared coordinator lock")
	networkPool := flag.String("network-pool", "10.240.0.0/16", "local per-account runner subnet pool")
	guestPool := flag.String("guest-network-pool", "10.241.0.0/16", "local per-account guest subnet pool")
	flag.Parse()
	if !*enabled {
		log.Print("VM helper disabled")
		return
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		log.Fatal("helper requires Linux root; runtime capabilities are checked on Docker server")
	}
	if !filepath.IsAbs(*socket) || !*enabled || *config == "" {
		log.Fatal("invalid helper configuration")
	}
	b, err := os.ReadFile(*config)
	if err != nil {
		log.Fatal("cannot read template configuration")
	}
	var templates map[string]vmruntime.Template
	if json.Unmarshal(b, &templates) != nil {
		log.Fatal("invalid template configuration")
	}
	helper, err := vmruntime.OpenHelper(*root, &vmruntime.DockerDriver{Templates: templates, NetworkCIDR: *networkPool, GuestNetworkCIDR: *guestPool, AllowTCG: *allowTCG, ServiceGID: *serviceGID})
	if err != nil {
		log.Fatal("cannot acquire helper state lock")
	}
	defer helper.Close()
	if err = helper.ConfigureServiceGroup(*serviceGID); err != nil {
		log.Fatal("cannot configure service group")
	}
	if err = os.MkdirAll(filepath.Dir(*socket), 0700); err != nil {
		log.Fatal("cannot prepare socket directory")
	}
	if err = vmruntime.PrepareCoordinatorLock(*socket, *serviceUID); err != nil {
		log.Fatal("cannot prepare application coordinator lock: ", err)
	}
	// With the exclusive state lock held, an existing socket may be stale. Do
	// not remove regular files or symlinks supplied as a socket path.
	if st, e := os.Lstat(*socket); e == nil {
		if st.Mode()&os.ModeSocket == 0 {
			log.Fatal("socket path is not a socket")
		}
		if c, e := net.DialTimeout("unix", *socket, time.Second); e == nil {
			c.Close()
			log.Fatal("helper socket already active")
		}
		if os.Remove(*socket) != nil {
			log.Fatal("cannot remove stale socket")
		}
	}
	listener, err := net.Listen("unix", *socket)
	if err != nil {
		log.Fatal("cannot bind helper socket")
	}
	defer listener.Close()
	defer os.Remove(*socket)
	socketMode := os.FileMode(0600)
	if *serviceGID > 0 {
		if err = os.Chown(filepath.Dir(*socket), 0, *serviceGID); err != nil {
			log.Fatal("cannot configure socket directory group")
		}
		if err = os.Chmod(filepath.Dir(*socket), 0710); err != nil {
			log.Fatal("cannot protect socket directory")
		}
		if err = os.Chown(*socket, 0, *serviceGID); err != nil {
			log.Fatal("cannot configure socket group")
		}
		socketMode = 0660
	}
	if os.Chmod(*socket, socketMode) != nil {
		log.Fatal("cannot protect helper socket")
	}
	srv := &http.Server{Handler: helper.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go func() {
		<-ctx.Done()
		stop, c := context.WithTimeout(context.Background(), 30*time.Second)
		defer c()
		srv.Shutdown(stop)
	}()
	if err = srv.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Print("helper server stopped unexpectedly")
	}
}
