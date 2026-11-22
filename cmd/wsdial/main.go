package main

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	wsHost := "qbs-cell41e1e2f6-3570-4dd1-ab91-d114ab440fd9.dp.appintegrations.us-east-1.prod.plato.ai.aws.dev"
	u := fmt.Sprintf("wss://%s/streaming/quickwork-chat-bidirectional-stream?surfaceType=QUICK_WORK_DESKTOP_WINDOWS&token=DUMMY&outpostId=x&workingSessionId=x&channel=prod", wsHost)
	d := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		Proxy:            http.ProxyFromEnvironment,
	}
	h := http.Header{}
	h.Set("Origin", "https://quick.aws.com")
	conn, resp, err := d.Dial(u, h)
	fmt.Println("err:", err)
	if resp != nil {
		fmt.Println("status:", resp.Status)
		for k, v := range resp.Header {
			fmt.Printf("%s: %v\n", k, v)
		}
		buf := make([]byte, 400)
		n, _ := resp.Body.Read(buf)
		fmt.Println("body:", string(buf[:n]))
	}
	if conn != nil {
		conn.Close()
	}
}
