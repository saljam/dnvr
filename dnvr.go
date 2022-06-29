// Command dnvr is a dumb nvr.
//
// go run . -config sources.json -ffmpeg "docker exec -i toolbox ffmpeg" -debug
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"image"
	"image/png"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/deepch/vdk/av"
	"github.com/deepch/vdk/codec/h264parser"
	"github.com/deepch/vdk/format/rtsp"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
)

var (
	debug         bool
	outDir        string
	ffmpegCommand = []string{}
	config        = struct {
		Sources map[string]struct {
			URL    string
			Record bool
			Motion float64
			ACL    []netip.Prefix
		}
	}{}
)

var cameras = map[string]*camera{}

type camera struct {
	id        string
	src       string
	track     *webrtc.TrackLocalStaticSample
	ffin      io.Writer
	ffout     io.Reader
	threshold float64 // TODO it would be nice to "autotune" this.
	acl       []netip.Prefix

	// object lock protects concurrent access to all three following
	// fields. they are independent.
	sync.RWMutex
	record       io.Writer
	datachannels []*webrtc.DataChannel
	motion       float64
}

var index = template.Must(template.New("index").Parse(`
<!doctype html>
<html dir="auto">
<meta charset=utf-8>
<style>
body {
	padding: 0;
	margin: 0;
	background: black;
	display: grid;
	grid-template-columns: repeat(3, 1fr);
	border: solid 4px black;
	box-sizing: border-box;
}
video {
	width: 100%;
	height: 100%;
	object-fit: fill;
}
.video:first-child {
	grid-area: 1/1/3/3; // 1/1/3/4 for 4?
}
.video {
	width: 100%;
	height: 100%;
	object-fit: fill;
	box-sizing: border-box;
	position: relative;
}
.online {}
.offline {
	border: solid 10px red;
}
.video span {
	position: absolute;
	top: 0;
	right: 0;
	font-family: monospace;color: #ffc825;
	text-shadow: 0px 0px 10px black;
	margin: 15px;
}
.video.moving span:before {
	content: "👋";
	margin: 4px;
}
@media (max-width: 1000px) {
	body {
		grid-template-columns: 1fr;
	}
	.video:first-child {
		grid-area: unset;
	}
	.video {
		height: auto;
	}
}
</style>

<script>
let debug = true;
let sources = {{.}};
let draggedVideo = null;

function handleDragEnd(e) {
	this.style.opacity = '1';
}

function handleDragStart(e) {
	this.style.opacity = '0.4';
	draggedVideo = this;
}

function handleDrop(e) {
	e.stopPropagation();
	const anchor = this.nextElementSibling;
	if (draggedVideo === this) {
		return false
	}
	if (draggedVideo === anchor) {
		this.parentNode.insertBefore(draggedVideo, this);
		return false
	}
	this.parentNode.insertBefore(this, draggedVideo);
	this.parentNode.insertBefore(draggedVideo, anchor);
	return false;
}

function handleDragOver(e) {
	e.preventDefault();
}

async function connect(id, pc) {
	let offer = await pc.createOffer();
	pc.setLocalDescription(offer);
	console.log(id + " offer: ");
	console.log(offer.sdp);
	// TODO use template string once this is out of the go source file.
	let res = await fetch('/'+id, {method: 'post', body: JSON.stringify(offer)});
	let answer = await res.json();
	await pc.setRemoteDescription(answer);
	console.log(id + " answer: ");
	console.log(answer.sdp);
	await pc.addIceCandidate(null);
}

function addVideo(id) {
	let pc = new RTCPeerConnection();
	pc.addTransceiver('video');

	let v = document.createElement("video");
	v.setAttribute("playsinline", ""); 
	v.autoplay = true;
	v.muted = true;
	v.controls = true;

	let div = document.createElement("div");
	div.id = id;
	div.classList.add("video");
	div.draggable = true;
	div.addEventListener('dragstart', handleDragStart, false);
	div.addEventListener('dragend', handleDragEnd, false);
	div.addEventListener('dragover', handleDragOver, false);
	div.addEventListener('drop', handleDrop);
	div.appendChild(v);
	let span = document.createElement("span");
	div.appendChild(span);
	document.body.appendChild(div);

	pc.oniceconnectionstatechange = () => {
		console.log(id + ": " + pc.iceConnectionState)
		if (pc.iceConnectionState === "connected" || pc.iceConnectionState === "completed") {
			div.classList.add("online");
			div.classList.remove("offline");
		} else {
			div.classList.remove("online");
			div.classList.add("offline");
		}
	}

	pc.ontrack = e => {
		// TODO check if it's actually a video. do something about audio?
		v.srcObject = e.streams[0];
	}

	let dc = pc.createDataChannel("d");
	dc.onmessage = e => {
		let msg = JSON.parse(e.data);
		if (msg.Motion > msg.Threshold) {
			div.classList.add("moving");
		} else {
			div.classList.remove("moving");
		}
		span.innerText = id;
		if (debug) {
			span.innerText += " (" + msg.Threshold + ") " + msg.Motion.toFixed(2);
		}
	};

	connect(id, pc);

	return {
		id: id,
		v: v,
		div: div,
		span: span,
		pc: pc,
		dc: dc,
	}
}

let cameras = [];

function main() {
	sources.sort();
	for (const i in sources) {
		cameras.push(addVideo(sources[i]));
	}
}

document.addEventListener("DOMContentLoaded", main);
</script>
<body>
`))

func answer(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Path[1:]

	c, ok := cameras[id]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if !c.addrAllowed(r.RemoteAddr) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	var offer webrtc.SessionDescription
	err := json.NewDecoder(r.Body).Decode(&offer)
	if err != nil {
		http.Error(w, "bad times", http.StatusInternalServerError)
		return
	}

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		http.Error(w, "bad times", http.StatusInternalServerError)
		return
	}

	err = pc.SetRemoteDescription(offer)
	if err != nil {
		http.Error(w, "bad times", http.StatusInternalServerError)
		return
	}

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		c.Lock()
		c.datachannels = append(c.datachannels, dc)
		c.Unlock()
	})

	_, err = pc.AddTrack(c.track)
	if err != nil {
		http.Error(w, "bad times", http.StatusInternalServerError)
		return
	}

	gatherCandidates := webrtc.GatheringCompletePromise(pc)

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		http.Error(w, "bad times", http.StatusInternalServerError)
		return
	}

	err = pc.SetLocalDescription(answer)
	if err != nil {
		http.Error(w, "bad times", http.StatusInternalServerError)
		return
	}

	<-gatherCandidates

	buf, err := json.Marshal(pc.LocalDescription())
	if err != nil {
		http.Error(w, "bad times", http.StatusInternalServerError)
		return
	}

	w.Write(buf)
}

func (c *camera) addrAllowed(addr string) bool {
	if len(c.acl) == 0 {
		return true
	}
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		log.Printf("could not check against acl: %v", err)
		return false
	}
	for _, prefix := range c.acl {
		if prefix.Contains(ap.Addr()) {
			return true
		}
	}
	log.Printf("addr %v not in acl %v", ap.Addr(), c.acl)
	return false
}

func serve(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodHead:
	case http.MethodGet:
		var ids []string
		for id, c := range cameras {
			if c.addrAllowed(r.RemoteAddr) {
				ids = append(ids, id)
			}
		}
		index.Execute(w, ids)
		log.Printf("%s	%s	%s\n", r.RemoteAddr, r.Method, r.URL)
	case http.MethodPost:
		answer(w, r)
	default:
		http.Error(w, "unknown method", http.StatusMethodNotAllowed)
	}
}

func (c *camera) broadcast(ctx context.Context) {
	for {
		time.Sleep(2 * time.Second)
		select {
		case <-ctx.Done():
			return
		default:
		}

		c.RLock()
		// answer() only appends, so copying slice header is fine here
		dcs := c.datachannels
		stat := struct {
			Motion    float64
			Threshold float64
		}{
			Motion:    c.motion,
			Threshold: c.threshold,
		}
		c.RUnlock()

		buf, err := json.Marshal(stat)
		if err != nil {
			// We don't expect this to fix itself, so bail out here.
			// Fatalf might also be appropriate here.
			log.Printf("unexpected error from json.Marshal: %v", err)
			return
		}

		if len(dcs) == 0 {
			continue
		}

		for i := 0; i < len(dcs); i++ {
			err := dcs[i].SendText(string(buf))
			if err != nil {
				dcs = append(dcs[:i], dcs[i+1:]...)
				c.Lock()
				c.datachannels = append(c.datachannels[:i], c.datachannels[i+1:]...)
				c.Unlock()
			}
		}
	}
}

func (c *camera) runffmpeg(ctx context.Context) error {
	if len(ffmpegCommand) == 0 {
		return errors.New("ffmpeg disabled")
	}
	cmd := exec.CommandContext(ctx, ffmpegCommand[0], append(ffmpegCommand[1:],
		"-f", "h264",
		"-i", "-",
		"-vf", "scale=320:240,edgedetect",
		"-vcodec", "rawvideo",
		"-pix_fmt", "gray",
		"-f", "rawvideo", "-",
	)...)
	var err error
	c.ffin, err = cmd.StdinPipe()
	if err != nil {
		return err
	}
	c.ffout, err = cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if debug {
		fferr, err := cmd.StderrPipe()
		if err != nil {
			return err
		}
		go io.Copy(os.Stderr, fferr)
	}
	err = cmd.Start()
	if err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

func (c *camera) dumpFrame(buf []byte, filename string) error {
	img := &image.Gray{
		Pix:    buf,
		Stride: 320 * 1,
		Rect:   image.Rect(0, 0, 320, 240),
	}
	f, err := os.Create(filename)
	if err != nil {
		return err
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func (c *camera) detectMotion(ctx context.Context, r io.Reader) {
	buf := make([]byte, 320*240*1)
	prev := make([]byte, 320*240*1)
	movingFrames := 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_, err := io.ReadFull(r, buf)
		if err != nil {
			log.Printf("motion: could not read frame pixels: %v", err)
			return
		}

		// It turns out there's a name for this. It's SAD.
		// https://en.wikipedia.org/wiki/Sum_of_absolute_differences

		sum := 0
		for i := range buf {
			a, b := buf[i], prev[i]
			if a < b {
				sum += int(b - a)
			} else {
				sum += int(a - b)
			}
		}

		if debug {
			c.dumpFrame(buf, "image.png")
		}
		prev, buf = buf, prev

		motion := float64(sum) / float64(320*240)

		c.Lock()
		c.motion = motion
		c.Unlock()

		if motion > c.threshold {
			movingFrames++
		} else {
			movingFrames = 0
		}

		c.RLock()
		recording := c.record != io.Discard
		c.RUnlock()

		// We're moving and not recording. Start recording.
		if movingFrames > 5 && !recording {
			log.Printf("motion in %v", c.id)
			// TODO always keep N frames in some ring buffer to record
			// a few seconds before motion. can probably have a Writer
			// in c.record that does that and wrap the ffmpeg pipe or
			// Discard.
			c.startRecording(ctx, 1*time.Minute)
		}
	}
}

func (c *camera) newRecordingFilename() (string, error) {
	now := time.Now()
	dir := filepath.Join(outDir, now.Format("2006-01-02"))
	err := os.MkdirAll(dir, 0700)
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%s.mp4", time.Now().Format("150405"), c.id)
	return filepath.Join(dir, name), nil
}

func (c *camera) startRecording(ctx context.Context, duration time.Duration) {
	ctx, cancel := context.WithCancel(ctx)

	name, err := c.newRecordingFilename()
	if err != nil {
		log.Println("could not start recording %v in %v: %v", name, outDir, err)
		c.record = io.Discard
		return
	}

	if len(ffmpegCommand) == 0 {
		// no ffmpeg
		return
	}

	cmd := exec.CommandContext(ctx, ffmpegCommand[0], append(ffmpegCommand[1:],
		"-f", "h264",
		"-r", "10",
		"-framerate", "10",
		"-i", "-",
		"-r", "10",
		"-framerate", "10",
		name,
	)...)
	ffpipe, err := cmd.StdinPipe()
	if err != nil {
		log.Println("could not start recording %v: %v", name, err)
		// If we got here something is messed up - ffmpeg is broken or
		c.record = io.Discard
		return
	}
	err = cmd.Start()
	if err != nil {
		log.Println("could not start recording %v: %v", name, err)
		c.record = io.Discard
		return
	}

	c.Lock()
	defer c.Unlock()
	c.record = ffpipe

	go func() {
		time.Sleep(duration)
		c.Lock()
		defer c.Unlock()
		c.record = io.Discard
		ffpipe.Close()
		go func() { time.Sleep(5 * time.Second); cancel() }()
		err := cmd.Wait()
		if err != nil {
			log.Println("error finishing recording %v: %v", name, err)
			c.record = io.Discard
		}
	}()
}

func (c *camera) stream(ctx context.Context) {
	suppresserrors := false
	for {
		ctx, cancel := context.WithCancel(ctx)

		c.ffin = ioutil.Discard
		c.ffout = nil
		if c.threshold != 0 && c.record != nil {
			err := c.runffmpeg(ctx)
			if err != nil {
				if !suppresserrors {
					log.Printf("could not start ffmpeg: %v", err)
					suppresserrors = true
				}
			} else {
				suppresserrors = false
				go c.detectMotion(ctx, c.ffout)
			}
		}

		c.readRTSP(ctx)
		cancel()
		time.Sleep(10 * time.Second)
	}
}

func (c *camera) readRTSP(ctx context.Context) {
	conn, err := rtsp.Dial(c.src)
	if err != nil {
		log.Printf("can not dial rtsp: %v", err)
		return
	}
	defer conn.Close()
	conn.RtpKeepAliveTimeout = 10 * time.Second

	streams, err := conn.Streams()
	if err != nil {
		log.Printf("can not get streams: %v", err)
		return
	}

	if len(streams) < 1 || streams[0].Type() != av.H264 {
		log.Printf("first for %s stream not usable", c.src)
		return
	}

	header := make([]byte, 0, 1500)
	header = append(header, []byte{0x00, 0x00, 0x00, 0x01}...)
	header = append(header, streams[0].(h264parser.CodecData).SPS()...)
	header = append(header, []byte{0x00, 0x00, 0x00, 0x01}...)
	header = append(header, streams[0].(h264parser.CodecData).PPS()...)
	header = append(header, []byte{0x00, 0x00, 0x00, 0x01}...)
	var position time.Duration

	for {
		p, err := conn.ReadPacket()
		if err != nil {
			log.Printf("can not read packet: %v", err)
			return
		}
		if p.Idx != 0 {
			continue
		}

		if len(header)+len(p.Data[4:]) > cap(header) {
			b := make([]byte, len(header), len(header)+len(p.Data[4:]))
			copy(b, header)
			header = b
		}

		// For WebRTC we're fine only doing this on key frames but ffmpeg seems
		// to like that all frames have sps & pps. Maybe there's a better way
		// to fix this.
		// TODO change ReadPacket() so it takes in a buffer to avoid this copy?
		buf := header
		buf = buf[:len(header)+len(p.Data[4:])]
		copy(buf[len(header):], p.Data[4:])

		// TODO combine both and tee to ffmpeg in detectMotion()
		_, err = c.ffin.Write(buf)
		if err != nil {
			log.Printf("can not write frame: %v", err)
			return
		}
		c.RLock()
		if c.record != nil {
			_, err = c.record.Write(buf)
		}
		c.RUnlock()
		if err != nil {
			log.Printf("can not write frame: %v", err)
			return
		}

		err = c.track.WriteSample(media.Sample{
			Data:     buf,
			Duration: p.Time - position,
		})
		if err != nil && err != io.ErrClosedPipe {
			log.Printf("can not write frame to webrtc track: %v", err)
			return
		}
		position = p.Time
	}
}

func main() {
	httpaddr := flag.String("http", ":http", "http listen address")
	configpath := flag.String("config", "./sources.json", "path to config file")
	ffmpegcmd := flag.String("ffmpeg", "ffmpeg", "command line to run ffmpeg")
	flag.BoolVar(&debug, "debug", false, "debug mode")
	flag.StringVar(&outDir, "dir", ".", "directory in which to record videos")
	flag.Parse()

	cfgbuf, err := ioutil.ReadFile(*configpath)
	if err != nil {
		log.Fatalf("could not read config file %s: %v", *configpath, err)
	}
	err = json.Unmarshal(cfgbuf, &config)
	if err != nil {
		log.Fatalf("could not parse config file %s: %v", *configpath, err)
	}

	ffmpegCommand = strings.Fields(*ffmpegcmd)

	ctx := context.Background()

	for id, src := range config.Sources {
		track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: "video/h264"}, "v", "v")
		if err != nil {
			log.Fatalf("could not make track for source %s: %v", id, err)
		}

		c := &camera{
			id:        id,
			src:       src.URL,
			track:     track,
			threshold: src.Motion,
			acl:       src.ACL,
		}

		if src.Record {
			c.record = ioutil.Discard
		}

		go c.stream(ctx)
		go c.broadcast(ctx)
		cameras[id] = c
	}

	http.Handle("/", http.HandlerFunc(serve))
	log.Fatal(http.ListenAndServe(*httpaddr, nil))
}
