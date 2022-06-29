package main

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"io/ioutil"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
)

type request struct {
	Proto      string
	Method     string
	RequestURI string
	URL        *url.URL
	Header     textproto.MIMEHeader
	Body       []byte
}

func readRequest(r *bufio.Reader) (*request, error) {
	tp := textproto.NewReader(r)

	req := &request{}

	line, err := tp.ReadLine()
	if err != nil {
		return nil, err
	}

	method, rest, ok1 := strings.Cut(line, " ")
	requestURI, proto, ok2 := strings.Cut(rest, " ")
	if !ok1 || !ok2 {
		return nil, fmt.Errorf("malformed request")
	}
	req.Method, req.RequestURI, req.Proto = method, requestURI, proto

	if req.URL, err = url.ParseRequestURI(requestURI); err != nil {
		return nil, err
	}

	req.Header, err = tp.ReadMIMEHeader()
	if err != nil {
		return nil, err
	}

	cl := req.Header.Get("content-length")
	if cl != "" {
		length, err := strconv.Atoi(cl)
		if err != nil {
			return nil, fmt.Errorf("malformed content-length: %v", cl)
		}
		req.Body, err = ioutil.ReadAll(io.LimitReader(r, int64(length)))
		if err != nil {
			return nil, fmt.Errorf("could not read body: %w", err)
		}
	}
	return req, nil
}

var headerNewlineToSpace = strings.NewReplacer("\n", " ", "\r", " ")

func (req *request) Write(w io.Writer) error {
	bw := bufio.NewWriter(w)

	_, err := fmt.Fprintf(bw, "%s %s RTSP/1.0\r\n", req.Method, req.URL.String())
	if err != nil {
		return err
	}

	for k, vs := range req.Header {
		if k == "Cseq" {
			// TODO replace MIMEHeader with something case sensitive
			k = "CSeq"
		}
		for _, v := range vs {
			fmt.Fprintf(bw, "%s: %s\r\n", k, v)
		}
	}

	fmt.Fprintf(bw, "\r\n")
	return bw.Flush()
	//TODO body
}

type response struct {
	Proto      string
	Status     string
	StatusCode int
	Header     textproto.MIMEHeader

	Body []byte
}

func readResponse(r *bufio.Reader) (*response, error) {
	tp := textproto.NewReader(r)

	resp := &response{}

	// Parse the first line of the response.
	line, err := tp.ReadLine()
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	proto, status, ok := strings.Cut(line, " ")
	if !ok {
		return nil, fmt.Errorf("malformed response: %v", line)
	}
	resp.Proto = proto
	resp.Status = strings.TrimLeft(status, " ")

	statusCode, _, _ := strings.Cut(resp.Status, " ")
	if len(statusCode) != 3 {
		return nil, fmt.Errorf("malformed status code: %v", statusCode)
	}
	resp.StatusCode, err = strconv.Atoi(statusCode)
	if err != nil || resp.StatusCode < 0 {
		return nil, fmt.Errorf("malformed status code: %v", statusCode)
	}

	// Parse the response headers.
	resp.Header, err = tp.ReadMIMEHeader()
	if err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}

	cl := resp.Header.Get("content-length")
	if cl != "" {
		length, err := strconv.Atoi(cl)
		if err != nil {
			return nil, fmt.Errorf("malformed content-length: %v", cl)
		}
		resp.Body, err = ioutil.ReadAll(io.LimitReader(r, int64(length)))
		if err != nil {
			return nil, fmt.Errorf("could not read body: %w", err)
		}
	}

	return resp, nil
}

func basicAuth(username, password string) string {
	auth := username + ":" + password
	return base64.StdEncoding.EncodeToString([]byte(auth))
}
