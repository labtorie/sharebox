// Command interop serves the real file API over the real tunnel protocol,
// with each WebSocket binary message standing in for one DataChannel
// message. The guest web app's test suite (apps/guest-web) connects to it
// to prove that the TypeScript and Go implementations interoperate
// byte-for-byte.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/coder/websocket"

	"sharebox/internal/fileserver"
	"sharebox/internal/fsroot"
	"sharebox/internal/security"
	"sharebox/internal/tunnel"
)

type wsLink struct{ c *websocket.Conn }

func (l wsLink) Send(b []byte) error {
	return l.c.Write(context.Background(), websocket.MessageBinary, b)
}
func (l wsLink) BufferedAmount() uint64 { return 0 }
func (l wsLink) Close() error           { return l.c.Close(websocket.StatusNormalClosure, "") }

func main() {
	dir, _ := os.MkdirTemp("", "sbx-interop")
	os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello from go"), 0o644)
	big := make([]byte, 5<<20+123)
	for i := range big {
		big[i] = byte(i * 7)
	}
	os.WriteFile(filepath.Join(dir, "big.bin"), big, 0o644)
	root, err := fsroot.Open(dir)
	if err != nil {
		panic(err)
	}
	key := security.RandomToken(32)
	files, err := fileserver.New(fileserver.Options{Root: root, Config: fileserver.Config{
		Perms: fileserver.DefaultPermissions(), Limits: fileserver.DefaultLimits(), AccessKey: key}})
	if err != nil {
		panic(err)
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		c.SetReadLimit(1 << 20)
		sess := tunnel.New(wsLink{c}, tunnel.Options{Handler: files})
		for {
			_, b, err := c.Read(context.Background())
			if err != nil {
				sess.Close(err)
				return
			}
			sess.Deliver(b)
		}
	})
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	fmt.Printf("READY %d %s %s\n", ln.Addr().(*net.TCPAddr).Port, key, dir)
	http.Serve(ln, h)
}
