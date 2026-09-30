package fuel

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"sort"
)

var (
	errUnsupported = errors.New("unsupported media")
	errTooBig      = errors.New("too large")
)

const (
	maxMegapixels = 30_000_000
	modelLongEdge = 1568
)

// processedImage is an accepted photo: the stored copy (full size, oriented,
// metadata stripped) and the model copy (scaled to 1568 px on the long edge).
type processedImage struct {
	Stored []byte
	Model  []byte
	W, H   int
}

// sniffImage accepts JPEG or PNG by content (magic bytes), never by header.
func sniffImage(b []byte) (string, bool) {
	switch {
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "jpeg", true
	case len(b) >= 8 && bytes.Equal(b[:8], []byte("\x89PNG\r\n\x1a\n")):
		return "png", true
	}
	return "", false
}

// processImage validates, orients, strips and scales one upload (spec 3).
func processImage(b []byte) (*processedImage, error) {
	kind, ok := sniffImage(b)
	if !ok {
		return nil, errUnsupported
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil || (format != "jpeg" && format != "png") {
		return nil, errUnsupported
	}
	if int64(cfg.Width)*int64(cfg.Height) > maxMegapixels {
		return nil, errTooBig
	}
	var img image.Image
	if kind == "jpeg" {
		img, err = jpeg.Decode(bytes.NewReader(b))
	} else {
		img, err = png.Decode(bytes.NewReader(b))
	}
	if err != nil {
		return nil, errUnsupported
	}
	orient := 1
	if kind == "jpeg" {
		orient = jpegOrientation(b)
	}
	oriented := applyOrientation(img, orient)
	var stored bytes.Buffer
	if err := jpeg.Encode(&stored, oriented, &jpeg.Options{Quality: 88}); err != nil {
		return nil, err
	}
	small := scaleToLongEdge(oriented, modelLongEdge)
	var model bytes.Buffer
	if err := jpeg.Encode(&model, small, &jpeg.Options{Quality: 82}); err != nil {
		return nil, err
	}
	bnd := oriented.Bounds()
	return &processedImage{Stored: stored.Bytes(), Model: model.Bytes(), W: bnd.Dx(), H: bnd.Dy()}, nil
}

// jpegOrientation reads the EXIF Orientation tag (1..8) from APP1; 1 when
// absent or unreadable.
func jpegOrientation(b []byte) int {
	r := bytes.NewReader(b[2:])
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(r, hdr[:2]); err != nil {
			return 1
		}
		if hdr[0] != 0xFF {
			return 1
		}
		marker := hdr[1]
		if marker == 0xD8 || (marker >= 0xD0 && marker <= 0xD7) || marker == 0x01 {
			continue
		}
		if marker == 0xDA || marker == 0xD9 {
			return 1 // start of scan: no EXIF before the image data
		}
		if _, err := io.ReadFull(r, hdr[2:4]); err != nil {
			return 1
		}
		n := int(binary.BigEndian.Uint16(hdr[2:4])) - 2
		if n < 0 {
			return 1
		}
		seg := make([]byte, n)
		if _, err := io.ReadFull(r, seg); err != nil {
			return 1
		}
		if marker == 0xE1 && len(seg) > 14 && string(seg[:6]) == "Exif\x00\x00" {
			return exifOrientation(seg[6:])
		}
	}
}

func exifOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	off := int(bo.Uint32(t[4:8]))
	if off < 8 || off+2 > len(t) {
		return 1
	}
	n := int(bo.Uint16(t[off : off+2]))
	for i := 0; i < n; i++ {
		e := off + 2 + i*12
		if e+12 > len(t) {
			return 1
		}
		if bo.Uint16(t[e:e+2]) == 0x0112 {
			v := int(bo.Uint16(t[e+8 : e+10]))
			if v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}

// applyOrientation returns the upright image for an EXIF orientation.
func applyOrientation(src image.Image, o int) image.Image {
	if o <= 1 || o > 8 {
		return src
	}
	in := toRGBA(src)
	w, h := in.Rect.Dx(), in.Rect.Dy()
	var dst *image.RGBA
	if o >= 5 {
		dst = image.NewRGBA(image.Rect(0, 0, h, w))
	} else {
		dst = image.NewRGBA(image.Rect(0, 0, w, h))
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var dx, dy int
			switch o {
			case 2:
				dx, dy = w-1-x, y
			case 3:
				dx, dy = w-1-x, h-1-y
			case 4:
				dx, dy = x, h-1-y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = h-1-y, x
			case 7:
				dx, dy = h-1-y, w-1-x
			case 8:
				dx, dy = y, w-1-x
			}
			si := y*in.Stride + x*4
			di := dy*dst.Stride + dx*4
			copy(dst.Pix[di:di+4], in.Pix[si:si+4])
		}
	}
	return dst
}

// toRGBA converts through image/draw, which has fast paths for YCbCr.
func toRGBA(src image.Image) *image.RGBA {
	if r, ok := src.(*image.RGBA); ok && r.Rect.Min == (image.Point{}) {
		return r
	}
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Rect, src, b.Min, draw.Src)
	return dst
}

// scaleToLongEdge box-filters the image down so its long edge is at most max.
func scaleToLongEdge(src image.Image, max int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	long := w
	if h > long {
		long = h
	}
	if long <= max {
		return src
	}
	nw, nh := w*max/long, h*max/long
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	rgba := toRGBA(src)
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		y0, y1 := y*h/nh, (y+1)*h/nh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < nw; x++ {
			x0, x1 := x*w/nw, (x+1)*w/nw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, bl, a, n uint32
			for yy := y0; yy < y1; yy++ {
				off := yy*rgba.Stride + x0*4
				for xx := x0; xx < x1; xx++ {
					p := rgba.Pix[off : off+4]
					r += uint32(p[0])
					g += uint32(p[1])
					bl += uint32(p[2])
					a += uint32(p[3])
					n++
					off += 4
				}
			}
			dst.SetRGBA(x, y, color.RGBA{uint8(r / n), uint8(g / n), uint8(bl / n), uint8(a / n)})
		}
	}
	return dst
}

// ---- audio ----

// sniffAudio accepts AAC-in-MP4 (what AVAudioRecorder writes) or WAV by
// container, and returns the duration in seconds and a file extension.
func sniffAudio(b []byte) (ext string, seconds float64, err error) {
	switch {
	case len(b) >= 12 && string(b[4:8]) == "ftyp":
		d, ok := mp4Duration(b)
		if !ok {
			return "", 0, errUnsupported
		}
		return ".m4a", d, nil
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WAVE":
		d, ok := wavDuration(b)
		if !ok {
			return "", 0, errUnsupported
		}
		return ".wav", d, nil
	}
	return "", 0, errUnsupported
}

// mp4Boxes splits a buffer into its child boxes (type -> bodies in order).
func mp4Boxes(buf []byte) (map[string][][]byte, bool) {
	out := map[string][][]byte{}
	for i := 0; i < len(buf); {
		if i+8 > len(buf) {
			return nil, false
		}
		size := int(binary.BigEndian.Uint32(buf[i : i+4]))
		hdr := 8
		switch size {
		case 1:
			if i+16 > len(buf) {
				return nil, false
			}
			size64 := binary.BigEndian.Uint64(buf[i+8 : i+16])
			if size64 > uint64(len(buf)) {
				return nil, false
			}
			size, hdr = int(size64), 16
		case 0:
			size = len(buf) - i
		}
		if size < hdr || i+size > len(buf) {
			return nil, false
		}
		typ := string(buf[i+4 : i+8])
		out[typ] = append(out[typ], buf[i+hdr:i+size])
		i += size
	}
	return out, true
}

func mp4Child(buf []byte, typ string) []byte {
	m, ok := mp4Boxes(buf)
	if !ok || len(m[typ]) == 0 {
		return nil
	}
	return m[typ][0]
}

// mp4Duration validates an AAC-in-MP4 audio file (what AVAudioRecorder
// writes) and returns the SOUND TRACK's duration. It requires: ftyp; a
// non-empty mdat; in moov a trak whose hdlr is "soun", whose mdhd has a
// timescale, and whose stsd holds an mp4a entry with channels, a sample rate
// and an esds (AudioSpecificConfig); a sample table (stsz with samples,
// stco/co64) whose chunk offsets all point inside an mdat.
func mp4Duration(b []byte) (float64, bool) {
	// Top level, with absolute offsets for the mdat bounds check.
	type span struct{ lo, hi int }
	var mdats []span
	var moovBody []byte
	hasFtyp := false
	for i := 0; i < len(b); {
		if i+8 > len(b) {
			return 0, false
		}
		size := int(binary.BigEndian.Uint32(b[i : i+4]))
		hdr := 8
		switch size {
		case 1:
			if i+16 > len(b) {
				return 0, false
			}
			s64 := binary.BigEndian.Uint64(b[i+8 : i+16])
			if s64 > uint64(len(b)) {
				return 0, false
			}
			size, hdr = int(s64), 16
		case 0:
			size = len(b) - i
		}
		if size < hdr || i+size > len(b) {
			return 0, false // truncated
		}
		switch string(b[i+4 : i+8]) {
		case "ftyp":
			hasFtyp = true
		case "moov":
			moovBody = b[i+hdr : i+size]
		case "mdat":
			if size > hdr {
				mdats = append(mdats, span{i + hdr, i + size})
			}
		}
		i += size
	}
	if !hasFtyp || moovBody == nil || len(mdats) == 0 {
		return 0, false
	}
	moov, ok := mp4Boxes(moovBody)
	if !ok {
		return 0, false
	}
	if len(mdats) > 16 {
		return 0, false
	}
	// mdats are in file order (ascending): binary search, no nested scans.
	mdatAt := func(off uint64) (span, bool) {
		i := sort.Search(len(mdats), func(i int) bool { return uint64(mdats[i].hi) > off })
		if i < len(mdats) && off >= uint64(mdats[i].lo) {
			return mdats[i], true
		}
		return span{}, false
	}
	inMdat := func(off uint64) bool { _, ok := mdatAt(off); return ok }
	for _, trak := range moov["trak"] {
		mdia := mp4Child(trak, "mdia")
		hdlr := mp4Child(mdia, "hdlr")
		if len(hdlr) < 12 || string(hdlr[8:12]) != "soun" {
			continue
		}
		mdhd := mp4Child(mdia, "mdhd")
		var scale, dur uint64
		switch {
		case len(mdhd) >= 32 && mdhd[0] == 1:
			scale = uint64(binary.BigEndian.Uint32(mdhd[20:24]))
			dur = binary.BigEndian.Uint64(mdhd[24:32])
		case len(mdhd) >= 20 && mdhd[0] == 0:
			scale = uint64(binary.BigEndian.Uint32(mdhd[12:16]))
			dur = uint64(binary.BigEndian.Uint32(mdhd[16:20]))
		default:
			continue
		}
		if scale == 0 {
			continue
		}
		stbl := mp4Child(mp4Child(mdia, "minf"), "stbl")
		stsd := mp4Child(stbl, "stsd")
		// stsd: version/flags(4) count(4) then entry: size(4) "mp4a"(4)
		// reserved(6) dref(2) reserved(8) channels(2) bits(2) pre(2) res(2)
		// rate(4), then child boxes (esds).
		if len(stsd) < 8+36 || binary.BigEndian.Uint32(stsd[4:8]) < 1 || string(stsd[12:16]) != "mp4a" {
			continue
		}
		esz := int(binary.BigEndian.Uint32(stsd[8:12]))
		if esz < 36 || 8+esz > len(stsd) {
			continue
		}
		entry := stsd[8 : 8+esz]
		channels := binary.BigEndian.Uint16(entry[24:26])
		rate := binary.BigEndian.Uint32(entry[32:36]) >> 16
		if channels == 0 || channels > 8 || rate == 0 {
			continue
		}
		coreRate := validESDS(mp4Child(entry[36:], "esds"))
		if coreRate == 0 {
			continue
		}
		// Sample table: sizes (stsz), timing (stts) and chunks (stsc) must
		// agree with each other, with mdhd and with the mdat bytes.
		stsz := mp4Child(stbl, "stsz")
		if len(stsz) < 12 {
			continue
		}
		nSamples := uint64(binary.BigEndian.Uint32(stsz[8:12]))
		// 120 s of AAC at 96 kHz is under 11,300 frames: a larger table is
		// not a voice note (and bounds the validation work).
		if nSamples == 0 || nSamples > 20000 {
			continue
		}
		var sampleBytes uint64
		if uni := uint64(binary.BigEndian.Uint32(stsz[4:8])); uni != 0 {
			sampleBytes = uni * nSamples
		} else {
			if uint64(len(stsz)) < 12+4*nSamples {
				continue
			}
			for k := uint64(0); k < nSamples; k++ {
				sampleBytes += uint64(binary.BigEndian.Uint32(stsz[12+4*k:]))
			}
		}
		var mdatBytes uint64
		for _, m := range mdats {
			mdatBytes += uint64(m.hi - m.lo)
		}
		if sampleBytes == 0 || sampleBytes > mdatBytes {
			continue
		}
		stts := mp4Child(stbl, "stts")
		if len(stts) < 8 {
			continue
		}
		nEnt := int(binary.BigEndian.Uint32(stts[4:8]))
		if nEnt == 0 || 8+8*nEnt > len(stts) {
			continue
		}
		var tCount, ticks, maxDelta uint64
		for k := 0; k < nEnt; k++ {
			c := uint64(binary.BigEndian.Uint32(stts[8+8*k:]))
			d := uint64(binary.BigEndian.Uint32(stts[12+8*k:]))
			tCount += c
			ticks += c * d
			if d > maxDelta {
				maxDelta = d
			}
		}
		if tCount != nSamples || ticks == 0 {
			continue
		}
		if diff := int64(ticks) - int64(dur); diff > int64(maxDelta) || -diff > int64(maxDelta) {
			continue // mdhd duration disagrees with the sample timing
		}
		// Timing cross-check that does not trust the timescale: every AAC
		// frame carries 1024 samples at the core rate.
		byFrames := float64(nSamples) * 1024 / coreRate
		byTicks := float64(ticks) / float64(scale)
		if d := byTicks - byFrames; d > 0.1+0.05*byFrames || -d > 0.1+0.05*byFrames {
			continue
		}
		stsc := mp4Child(stbl, "stsc")
		if len(stsc) < 8 || binary.BigEndian.Uint32(stsc[4:8]) == 0 {
			continue
		}
		dur = ticks

		var offsets []uint64
		if stco := mp4Child(stbl, "stco"); len(stco) >= 8 {
			n := int(binary.BigEndian.Uint32(stco[4:8]))
			if n == 0 || uint64(n) > nSamples || 8+4*n > len(stco) {
				continue
			}
			for k := 0; k < n; k++ {
				offsets = append(offsets, uint64(binary.BigEndian.Uint32(stco[8+4*k:])))
			}
		} else if co64 := mp4Child(stbl, "co64"); len(co64) >= 8 {
			n := int(binary.BigEndian.Uint32(co64[4:8]))
			if n == 0 || uint64(n) > nSamples || 8+8*n > len(co64) {
				continue
			}
			for k := 0; k < n; k++ {
				offsets = append(offsets, binary.BigEndian.Uint64(co64[8+8*k:]))
			}
		} else {
			continue
		}
		good := true
		for _, off := range offsets {
			good = good && inMdat(off)
		}
		if !good {
			continue
		}
		// Chunk extents: walk the stsc runs ONCE (first chunks strictly
		// increasing from 1, positive samples per chunk, a valid description
		// index); every chunk's samples must fit inside the mdat its offset
		// points into, and the samples must add up exactly.
		sizes := func(k uint64) uint64 {
			if uni := uint64(binary.BigEndian.Uint32(stsz[4:8])); uni != 0 {
				return uni
			}
			return uint64(binary.BigEndian.Uint32(stsz[12+4*k:]))
		}
		nsc := int(binary.BigEndian.Uint32(stsc[4:8]))
		if 8+12*nsc > len(stsc) {
			continue
		}
		nChunks := uint64(len(offsets))
		var sample uint64
		// Validate EVERY run boundary before indexing offsets.
		extentsOK := nsc > 0
		prevFirst := uint64(0)
		for k := 0; k < nsc && extentsOK; k++ {
			first := uint64(binary.BigEndian.Uint32(stsc[8+12*k:]))
			per := uint64(binary.BigEndian.Uint32(stsc[12+12*k:]))
			sdi := binary.BigEndian.Uint32(stsc[16+12*k:])
			if (k == 0 && first != 1) || first <= prevFirst || first > nChunks || per == 0 || sdi == 0 {
				extentsOK = false
			}
			prevFirst = first
		}
		for k := 0; k < nsc && extentsOK; k++ {
			first := uint64(binary.BigEndian.Uint32(stsc[8+12*k:]))
			per := uint64(binary.BigEndian.Uint32(stsc[12+12*k:]))
			last := nChunks
			if k+1 < nsc {
				last = uint64(binary.BigEndian.Uint32(stsc[8+12*(k+1):])) - 1
			}
			for c := first; c <= last && extentsOK; c++ {
				if sample+per > nSamples {
					extentsOK = false // more samples claimed than stsz holds
					break
				}
				off := offsets[c-1]
				var ext uint64
				for x := uint64(0); x < per; x++ {
					ext += sizes(sample)
					sample++
				}
				m, ok := mdatAt(off)
				extentsOK = ok && off+ext <= uint64(m.hi)
			}
		}
		if !extentsOK || sample != nSamples {
			continue
		}
		return float64(dur) / float64(scale), true
	}
	return 0, false
}

// validESDS checks the MPEG-4 elementary stream descriptor of an mp4a
// entry: an ES descriptor holding a decoder config for MPEG-4 / MPEG-2 AAC
// audio with an AudioSpecificConfig of a known object type, a valid
// sampling-frequency index and channel configuration.
var aacRates = []float64{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

// validESDS returns the AAC core sample rate from the AudioSpecificConfig
// (0 when the descriptor is not valid AAC).
func validESDS(b []byte) float64 {
	if len(b) < 4 {
		return 0
	}
	p := b[4:] // version and flags
	readDesc := func(p []byte, tag byte) ([]byte, []byte, bool) {
		if len(p) < 2 || p[0] != tag {
			return nil, nil, false
		}
		n, i := 0, 1
		for ; i < 5 && i < len(p); i++ {
			n = n<<7 | int(p[i]&0x7f)
			if p[i]&0x80 == 0 {
				i++
				break
			}
		}
		if i+n > len(p) {
			return nil, nil, false
		}
		return p[i : i+n], p[i+n:], true
	}
	es, _, ok := readDesc(p, 0x03)
	if !ok || len(es) < 3 {
		return 0
	}
	flags := es[2]
	q := es[3:]
	if flags&0x80 != 0 { // streamDependenceFlag
		if len(q) < 2 {
			return 0
		}
		q = q[2:]
	}
	if flags&0x40 != 0 { // URL_Flag
		if len(q) < 1 || len(q) < 1+int(q[0]) {
			return 0
		}
		q = q[1+int(q[0]):]
	}
	if flags&0x20 != 0 { // OCRstreamFlag
		if len(q) < 2 {
			return 0
		}
		q = q[2:]
	}
	dc, _, ok := readDesc(q, 0x04)
	if !ok || len(dc) < 13 {
		return 0
	}
	switch dc[0] { // objectTypeIndication
	case 0x40, 0x66, 0x67, 0x68:
	default:
		return 0
	}
	if dc[1]>>2 != 0x05 { // streamType audio
		return 0
	}
	asc, _, ok := readDesc(dc[13:], 0x05)
	if !ok || len(asc) < 2 {
		return 0
	}
	aot := asc[0] >> 3
	freq := (asc[0]&0x07)<<1 | asc[1]>>7
	ch := (asc[1] >> 3) & 0x0f
	switch aot {
	case 1, 2, 3, 4, 5, 29:
	default:
		return 0
	}
	if freq > 12 || ch > 7 {
		return 0
	}
	return aacRates[freq]
}

// wavDuration validates a PCM (or float) WAV and returns data size / byte
// rate. A data chunk that claims more bytes than the file holds is truncated
// and rejected; the fmt chunk must be self-consistent.
func wavDuration(b []byte) (float64, bool) {
	riff := int(binary.LittleEndian.Uint32(b[4:8]))
	if riff+8 > len(b) || riff < 4 {
		return 0, false // the RIFF length claims more than the file holds
	}
	var byteRate uint32
	fmtOK := false
	for i := 12; i+8 <= riff+8; {
		id := string(b[i : i+4])
		size := int(binary.LittleEndian.Uint32(b[i+4 : i+8]))
		body := i + 8
		if size < 0 || body+size > len(b) || body+size > riff+8 {
			return 0, false // truncated chunk
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return 0, false
			}
			format := binary.LittleEndian.Uint16(b[body : body+2])
			channels := binary.LittleEndian.Uint16(b[body+2 : body+4])
			rate := binary.LittleEndian.Uint32(b[body+4 : body+8])
			byteRate = binary.LittleEndian.Uint32(b[body+8 : body+12])
			align := binary.LittleEndian.Uint16(b[body+12 : body+14])
			bits := binary.LittleEndian.Uint16(b[body+14 : body+16])
			if format == 0xFFFE {
				// WAVE_FORMAT_EXTENSIBLE: cbSize >= 22, valid bits <= bits,
				// SubFormat GUID naming PCM (1) or IEEE float (3).
				if size < 40 || binary.LittleEndian.Uint16(b[body+16:body+18]) < 22 {
					return 0, false
				}
				if valid := binary.LittleEndian.Uint16(b[body+18 : body+20]); valid == 0 || valid > bits {
					return 0, false
				}
				format = binary.LittleEndian.Uint16(b[body+24 : body+26])
			}
			switch {
			case format == 1 && (bits == 8 || bits == 16 || bits == 24 || bits == 32):
			case format == 3 && (bits == 32 || bits == 64):
			default:
				return 0, false
			}
			if channels == 0 || channels > 8 || rate == 0 || align != channels*bits/8 || byteRate != rate*uint32(align) {
				return 0, false
			}
			fmtOK = true
		case "data":
			if !fmtOK || byteRate == 0 || size == 0 {
				return 0, false
			}
			return float64(size) / float64(byteRate), true
		}
		i = body + size + size%2
	}
	return 0, false
}
