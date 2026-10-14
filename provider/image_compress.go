package provider

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
	"velocitylabs.cc/sdk"
)

type TargetSize int64

func (t *TargetSize) UnmarshalYAML(value *yaml.Node) error {
	if value == nil || value.Kind == 0 {
		*t = 0
		return nil
	}
	parsed, err := parseTargetSizeString(value.Value)
	if err != nil {
		return err
	}
	*t = TargetSize(parsed)
	return nil
}

func (t *TargetSize) UnmarshalJSON(data []byte) error {
	raw := strings.TrimSpace(string(data))
	if raw == "" || raw == "null" {
		*t = 0
		return nil
	}
	if strings.HasPrefix(raw, "\"") {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		raw = s
	}
	parsed, err := parseTargetSizeString(raw)
	if err != nil {
		return err
	}
	*t = TargetSize(parsed)
	return nil
}

func parseTargetSizeString(raw string) (int64, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" || s == "0" {
		return 0, nil
	}

	multiplier := float64(1024)
	switch {
	case strings.HasSuffix(s, "mib"):
		multiplier = 1024 * 1024
		s = strings.TrimSpace(strings.TrimSuffix(s, "mib"))
	case strings.HasSuffix(s, "mb"):
		multiplier = 1024 * 1024
		s = strings.TrimSpace(strings.TrimSuffix(s, "mb"))
	case strings.HasSuffix(s, "m"):
		multiplier = 1024 * 1024
		s = strings.TrimSpace(strings.TrimSuffix(s, "m"))
	case strings.HasSuffix(s, "kib"):
		multiplier = 1024
		s = strings.TrimSpace(strings.TrimSuffix(s, "kib"))
	case strings.HasSuffix(s, "kb"):
		multiplier = 1024
		s = strings.TrimSpace(strings.TrimSuffix(s, "kb"))
	case strings.HasSuffix(s, "k"):
		multiplier = 1024
		s = strings.TrimSpace(strings.TrimSuffix(s, "k"))
	case strings.HasSuffix(s, "bytes"):
		multiplier = 1
		s = strings.TrimSpace(strings.TrimSuffix(s, "bytes"))
	case strings.HasSuffix(s, "b"):
		multiplier = 1
		s = strings.TrimSpace(strings.TrimSuffix(s, "b"))
	}

	val, err := strconv.ParseFloat(s, 64)
	if err != nil || val < 0 {
		return 0, fmt.Errorf("invalid target_size %q", raw)
	}
	return int64(math.Round(val * multiplier)), nil
}

type ImageCompression struct {
	Format     string     `yaml:"format,omitempty" json:"format,omitempty"`
	Quality    int        `yaml:"quality,omitempty" json:"quality,omitempty"`
	TargetSize TargetSize `yaml:"target_size,omitempty" json:"target_size,omitempty"`
}

func ResolveImageCompression(providerCfg, modelCfg ImageCompression) ImageCompression {
	out := providerCfg
	if strings.TrimSpace(modelCfg.Format) != "" {
		out.Format = modelCfg.Format
	}
	if modelCfg.Quality > 0 {
		out.Quality = modelCfg.Quality
	}
	if modelCfg.TargetSize > 0 {
		out.TargetSize = modelCfg.TargetSize
	}
	return out.Normalized()
}

func (c ImageCompression) Normalized() ImageCompression {
	c.Format = normalizeImageFormat(c.Format)
	if c.Quality < 0 {
		c.Quality = 0
	} else if c.Quality > 100 {
		c.Quality = 100
	}
	if c.TargetSize < 0 {
		c.TargetSize = 0
	}
	return c
}

func (c ImageCompression) Enabled() bool {
	f := strings.ToLower(strings.TrimSpace(c.Format))
	if f == "none" || f == "off" || f == "raw" {
		return false
	}
	return f != "" || c.Quality > 0 || c.TargetSize > 0
}

func normalizeImageFormat(format string) string {
	switch strings.ToLower(strings.TrimSpace(strings.TrimPrefix(format, "."))) {
	case "", "auto":
		return ""
	case "none", "off", "raw":
		return "none"
	case "jpg", "jpeg", "image/jpeg", "image/jpg":
		return "jpeg"
	case "png", "image/png":
		return "png"
	case "webp", "image/webp":
		return "webp"
	case "avif", "image/avif":
		return "avif"
	case "heic", "heif", "image/heic", "image/heif":
		return "heic"
	default:
		return strings.ToLower(strings.TrimSpace(format))
	}
}

func FormatMIME(format string) string {
	switch normalizeImageFormat(format) {
	case "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "webp":
		return "image/webp"
	case "avif":
		return "image/avif"
	case "heic":
		return "image/heic"
	default:
		return ""
	}
}

func CompressImageBytes(data []byte, origMIME string, cfg ImageCompression) ([]byte, string) {
	cfg = cfg.Normalized()
	if !cfg.Enabled() || len(data) == 0 {
		return data, origMIME
	}
	if strings.EqualFold(origMIME, "image/svg+xml") {
		return data, origMIME
	}

	targetFormat := cfg.Format
	if targetFormat == "" {
		if cfg.TargetSize > 0 || cfg.Quality > 0 {
			targetFormat = "jpeg"
		} else {
			return data, origMIME
		}
	}
	targetMIME := FormatMIME(targetFormat)
	if targetMIME == "" {
		return data, origMIME
	}

	quality := cfg.Quality
	if quality <= 0 {
		quality = 82
	}
	targetBytes := int64(cfg.TargetSize)

	if targetBytes <= 0 {
		if out, ok := encodeImageOnce(data, targetFormat, quality, 100, 0); ok {
			return out, targetMIME
		}
		return data, origMIME
	}

	if int64(len(data)) <= targetBytes && (cfg.Format == "" || strings.EqualFold(origMIME, targetMIME)) && cfg.Quality <= 0 {
		return data, origMIME
	}

	var best []byte
	qualities := candidateQualities(quality)
	scales := []int{100, 85, 70, 55, 40, 25}

	for _, scale := range scales {
		for _, q := range qualities {
			out, ok := encodeImageOnce(data, targetFormat, q, scale, targetBytes)
			if !ok || len(out) == 0 {
				continue
			}
			if best == nil || len(out) < len(best) {
				best = out
			}
			if int64(len(out)) <= targetBytes {
				return out, targetMIME
			}
		}
		if targetFormat == "png" {
			break
		}
	}

	if best != nil {
		return best, targetMIME
	}
	return data, origMIME
}

func CompressImageDataURL(dataURL string, cfg ImageCompression) string {
	cfg = cfg.Normalized()
	if !cfg.Enabled() || !strings.HasPrefix(dataURL, "data:image/") {
		return dataURL
	}
	semi := strings.Index(dataURL, ";base64,")
	if semi < 0 {
		return dataURL
	}
	origMIME := dataURL[len("data:"):semi]
	rawB64 := dataURL[semi+len(";base64,"):]
	decoded, err := base64.StdEncoding.DecodeString(strings.Map(dropWhitespace, rawB64))
	if err != nil || len(decoded) == 0 {
		return dataURL
	}
	compressed, newMIME := CompressImageBytes(decoded, origMIME, cfg)
	if len(compressed) == 0 {
		return dataURL
	}
	return "data:" + newMIME + ";base64," + base64.StdEncoding.EncodeToString(compressed)
}

func candidateQualities(start int) []int {
	if start <= 0 {
		start = 82
	}
	steps := []int{start, 75, 65, 52, 40, 28, 18}
	out := make([]int, 0, len(steps))
	seen := map[int]bool{}
	for _, q := range steps {
		if q > start || q < 10 || seen[q] {
			continue
		}
		seen[q] = true
		out = append(out, q)
	}
	if len(out) == 0 {
		out = append(out, start)
	}
	return out
}

func encodeImageOnce(data []byte, targetFormat string, quality, scalePct int, targetBytes int64) ([]byte, bool) {
	switch targetFormat {
	case "jpeg", "png":
		if out, ok := encodeImageStdlib(data, targetFormat, quality, scalePct); ok {
			return out, true
		}
	}
	return encodeImageExternal(data, targetFormat, quality, scalePct, targetBytes)
}

func encodeImageStdlib(data []byte, targetFormat string, quality, scalePct int) ([]byte, bool) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, false
	}
	if scalePct > 0 && scalePct < 100 {
		img = scaleImageNearest(img, scalePct)
	}
	var buf bytes.Buffer
	switch targetFormat {
	case "jpeg":
		flat := flattenOntoWhite(img)
		if err := jpeg.Encode(&buf, flat, &jpeg.Options{Quality: quality}); err != nil {
			return nil, false
		}
	case "png":
		enc := png.Encoder{CompressionLevel: png.BestCompression}
		if err := enc.Encode(&buf, img); err != nil {
			return nil, false
		}
	default:
		return nil, false
	}
	return buf.Bytes(), true
}

func flattenOntoWhite(src image.Image) image.Image {
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			sr, sg, sb, sa := src.At(b.Min.X+x, b.Min.Y+y).RGBA()
			if sa == 0xffff {
				dst.SetRGBA(x, y, color.RGBA{R: uint8(sr >> 8), G: uint8(sg >> 8), B: uint8(sb >> 8), A: 0xff})
				continue
			}
			if sa == 0 {
				dst.SetRGBA(x, y, color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff})
				continue
			}
			invA := 0xffff - sa
			r := (sr + 0xffff*invA/0xffff) >> 8
			g := (sg + 0xffff*invA/0xffff) >> 8
			bl := (sb + 0xffff*invA/0xffff) >> 8
			if r > 255 {
				r = 255
			}
			if g > 255 {
				g = 255
			}
			if bl > 255 {
				bl = 255
			}
			dst.SetRGBA(x, y, color.RGBA{R: uint8(r), G: uint8(g), B: uint8(bl), A: 0xff})
		}
	}
	return dst
}

func scaleImageNearest(src image.Image, scalePct int) image.Image {
	b := src.Bounds()
	w := b.Dx() * scalePct / 100
	h := b.Dy() * scalePct / 100
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		sy := b.Min.Y + y*b.Dy()/h
		for x := 0; x < w; x++ {
			sx := b.Min.X + x*b.Dx()/w
			dst.Set(x, y, src.At(sx, sy))
		}
	}
	return dst
}

func encodeImageExternal(data []byte, targetFormat string, quality, scalePct int, targetBytes int64) ([]byte, bool) {
	tmpDir, err := os.MkdirTemp("", sdk.Current().TempPrefix+"img-*")
	if err != nil {
		return nil, false
	}
	defer os.RemoveAll(tmpDir)

	inPath := filepath.Join(tmpDir, "in.img")
	if err := os.WriteFile(inPath, data, 0o600); err != nil {
		return nil, false
	}
	outPath := filepath.Join(tmpDir, "out."+targetFormat)

	if magickBin, err := exec.LookPath("magick"); err == nil {
		if out, ok := runMagick(magickBin, inPath, outPath, targetFormat, quality, scalePct, targetBytes); ok {
			return out, true
		}
	} else if convertBin, err := exec.LookPath("convert"); err == nil {
		if out, ok := runMagick(convertBin, inPath, outPath, targetFormat, quality, scalePct, targetBytes); ok {
			return out, true
		}
	}

	if scalePct == 100 {
		switch targetFormat {
		case "webp":
			if bin, err := exec.LookPath("cwebp"); err == nil {
				args := []string{"-quiet", "-q", strconv.Itoa(quality)}
				if targetBytes > 0 {
					args = append(args, "-size", strconv.FormatInt(targetBytes, 10))
				}
				args = append(args, inPath, "-o", outPath)
				if out, ok := execAndRead(bin, args, outPath); ok {
					return out, true
				}
			}
		case "avif":
			if bin, err := exec.LookPath("avifenc"); err == nil {
				args := []string{"-q", strconv.Itoa(quality), "-s", "6", inPath, outPath}
				if out, ok := execAndRead(bin, args, outPath); ok {
					return out, true
				}
			}
		case "heic":
			if bin, err := exec.LookPath("heif-enc"); err == nil {
				args := []string{"-q", strconv.Itoa(quality), inPath, "-o", outPath}
				if out, ok := execAndRead(bin, args, outPath); ok {
					return out, true
				}
			}
		}
	}

	if ffmpegBin, err := exec.LookPath("ffmpeg"); err == nil {
		args := []string{"-hide_banner", "-loglevel", "error", "-y", "-i", inPath, "-frames:v", "1"}
		if scalePct > 0 && scalePct < 100 {
			args = append(args, "-vf", fmt.Sprintf("scale=iw*%d/100:ih*%d/100", scalePct, scalePct))
		}
		if targetFormat == "webp" {
			args = append(args, "-quality", strconv.Itoa(quality))
		}
		args = append(args, outPath)
		if out, ok := execAndRead(ffmpegBin, args, outPath); ok {
			return out, true
		}
	}

	return nil, false
}

func runMagick(bin, inPath, outPath, targetFormat string, quality, scalePct int, targetBytes int64) ([]byte, bool) {
	args := []string{inPath + "[0]"}
	if scalePct > 0 && scalePct < 100 {
		args = append(args, "-resize", fmt.Sprintf("%d%%", scalePct))
	}
	if quality > 0 {
		args = append(args, "-quality", strconv.Itoa(quality))
	}
	if targetBytes > 0 && scalePct == 100 {
		switch targetFormat {
		case "webp":
			args = append(args, "-define", fmt.Sprintf("webp:target-size=%d", targetBytes))
		case "jpeg":
			kb := targetBytes / 1024
			if kb < 1 {
				kb = 1
			}
			args = append(args, "-define", fmt.Sprintf("jpeg:extent=%dkb", kb))
		}
	}
	args = append(args, filepath.Clean(outPath))
	return execAndRead(bin, args, outPath)
}

func execAndRead(bin string, args []string, outPath string) ([]byte, bool) {
	cmd := exec.Command(bin, args...)
	if err := cmd.Run(); err != nil {
		return nil, false
	}
	out, err := os.ReadFile(outPath)
	if err != nil || len(out) == 0 {
		return nil, false
	}
	return out, true
}
