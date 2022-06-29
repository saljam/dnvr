package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"strings"
)

// rtsp proxy to allow things that can't speak webrtc access to the
// cameras, but we still do auth, nice names, etc.

func rtspProxyListenAndServe(addr string) error {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	for {
		c, err := l.Accept()
		if err != nil {
			log.Printf("accept error: %v", err.Error())
			continue
		}
		go proxyRTSP(c)
	}
}

func proxyRTSP(src net.Conn) {
	var dst net.Conn
	var dstURL *url.URL

	r := bufio.NewReader(src)
	for {
		req, err := readRequest(r)
		if err != nil {
			log.Printf("could not parse request: %v", err)
			src.Close()
			return
		}

		log.Println(req)

		if req.Method == "OPTIONS" || req.Method == "DESCRIBE" {
			id := strings.TrimPrefix(req.URL.Path, "/")
			c, ok := cameras[id]
			if !ok {
				src.Close()
				log.Printf("could not find camera: %v", id)
				return
			}

			dstURL, err = url.Parse(c.src)
			if err != nil {
				src.Close()
				log.Printf("could not parse camera url: %v", c.src)
				return
			}
			req.URL.Host = dstURL.Host
			req.URL.Path = dstURL.Path
			if dst == nil {
				dst, err = net.Dial("tcp", dstURL.Host)
				if err != nil {
					log.Printf("could dial camera: %v", err)
					src.Close()
					return
				}
				go io.Copy(src, dst) // we just copy response bytes verbatum
			}
		}

		if dst == nil {
			src.Close()
			log.Printf("could not determine destination")
			return
		}

		//	usr := "admin"
		//	pwd := "99kcDdHWxFEWtzC"
		usr := dstURL.User.Username()
		pwd, _ := dstURL.User.Password()
		req.Header.Set("Authorization", fmt.Sprintf("Basic %s", basicAuth(usr, pwd)))
		req.Write(dst)
	}
}
