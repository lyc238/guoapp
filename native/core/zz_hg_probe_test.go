package core

import (
	"context"
	"crypto/tls"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestProbeHongguoPlayback 红果取流诊断探针（真网，默认跳过）。
// 下次红果再动接口时用它定位，不必从头排查：
//
//	HG_PROBE=1 HG_VIDEO_ID=<分集视频ID> go test ./core/ -run TestProbeHongguoPlayback -v
//
// 它会依次打旧端点 /novel/player/video_model/v1/ 与新端点
// /novel/player/multi_video_model/preload/v1/，打印各自的响应结构与选流结果。
func TestProbeHongguoPlayback(t *testing.T) {
	if os.Getenv("HG_PROBE") != "1" {
		t.Skip("需设置 HG_PROBE=1 才会打真网")
	}
	videoID := os.Getenv("HG_VIDEO_ID")
	if videoID == "" {
		t.Skip("需设置 HG_VIDEO_ID 指定分集视频 ID")
	}
	d := hongguoProbeDownloader(t)

	// 1) 旧端点（2026-10-19 起预期空响应：code 0 但无 video_model）
	oldPayload := map[string]any{
		"video_id": videoID, "content_type": 1,
		"biz_param": map[string]any{"need_all_video_definition": true, "video_platform": 3},
	}
	oldResult, oldErr := d.hongguoAppRequest(
		context.Background(), http.MethodPost, "/novel/player/video_model/v1/", nil, oldPayload)
	t.Logf("video_model/v1  err=%v code=%v dataKeys=%v",
		oldErr, mapCode(oldResult), mapKeys(nestedMap(oldResult, "data")))

	// 2) 新端点（video_platform=1024，Mixed 才放行）
	newPayload := map[string]any{
		"content_type": 1, "dr_scene": "preload",
		"mixed_video_id_map": map[string]any{"0": []string{videoID}},
		"biz_param":          map[string]any{"need_all_video_definition": true, "video_platform": 1024},
	}
	newResult, newErr := d.hongguoAppRequest(
		context.Background(), http.MethodPost, "/novel/player/multi_video_model/preload/v1/", nil, newPayload)
	data := nestedMap(newResult, "data")
	t.Logf("multi_video_model/preload  err=%v code=%v dataKeys=%v",
		newErr, mapCode(newResult), mapKeys(data))
	if newErr == nil {
		entry := nestedMap(data, videoID)
		if len(entry) == 0 {
			entry = nestedMap(data, "0")
		}
		model := decodeHongguoVideoModel(entry)
		t.Logf("  model keys=%v  has video_list=%v", mapKeys(model), model["video_list"] != nil)
	}

	// 3) 完整选流链路（resolveHongguoAppMedia → multi_video_model + 选流 + 密钥）
	media, resolveErr := d.resolveHongguoAppMedia(context.Background(), videoID)
	if resolveErr != nil {
		t.Logf("resolveHongguoAppMedia err: %v", resolveErr)
		t.FailNow()
	}
	t.Logf("resolve ok: quality=%d variants=%d cencKey=%v url=%s",
		media.Quality, len(media.Variants), media.CENCKey != nil, media.URL)
}

// hongguoProbeDownloader 构造一个打真网的 Downloader（默认红果 App 网关）。
func hongguoProbeDownloader(t *testing.T) *Downloader {
	t.Helper()
	d := &Downloader{
		cfg:           Config{dataDir: t.TempDir(), Retries: 2},
		providerHosts: map[string]string{},
		limiter:       newRequestLimiter(3, 250*time.Millisecond),
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	d.client = &http.Client{Transport: transport, Timeout: 30 * time.Second}
	return d
}

func mapCode(result map[string]any) any {
	if len(result) == 0 {
		return "<empty>"
	}
	return firstNonEmpty(mapString(result, "code", "Code", "status_code"), mapString(nestedMap(result, "BaseResp"), "StatusCode"), "<empty>")
}

func mapKeys(result map[string]any) []string {
	if len(result) == 0 {
		return nil
	}
	keys := make([]string, 0, len(result))
	for key := range result {
		keys = append(keys, key)
	}
	return keys
}
