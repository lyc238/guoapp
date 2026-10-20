package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (downloader *Downloader) resolveHongguoAppMedia(ctx context.Context, videoID string) (providerMedia, error) {
	if !hongguoNumericID.MatchString(videoID) {
		return providerMedia{}, errors.New("红果视频 ID 无效")
	}
	// 2026-10-19 起 /novel/player/video_model/v1/ 被服务端按端点收紧，
	// 对评论身份签名一律返回 200+空 body（三通道齐挂，目录/详情正常）。
	// 改走仍在放行的 multi_video_model/preload 端点，请求体与响应结构见下。
	payload := map[string]any{
		"content_type": 1,
		"dr_scene":     "preload",
		"mixed_video_id_map": map[string]any{
			"0": []string{videoID},
		},
		"biz_param": map[string]any{"need_all_video_definition": true, "video_platform": 1024},
	}
	result, err := downloader.hongguoAppRequest(ctx, http.MethodPost, "/novel/player/multi_video_model/preload/v1/", nil, payload)
	if err != nil {
		return providerMedia{}, err
	}
	data := nestedMap(result, "data")
	entry := nestedMap(data, videoID)
	if len(entry) == 0 {
		entry = nestedMap(data, "0")
	}
	model := decodeHongguoVideoModel(entry)
	if len(model) == 0 {
		return providerMedia{}, errors.New("红果 App 未返回该集可用的播放信息，请稍后重试")
	}
	return selectHongguoAppMedia(model)
}

// decodeHongguoVideoModel 从 multi_video_model 的单个 video 节点里取出
// video_model（可能是 map，也可能是 base64 编码的 JSON 串），返回选流所需的
// 平铺结构，供 selectHongguoAppMedia 消费。
func decodeHongguoVideoModel(entry map[string]any) map[string]any {
	model, _ := entry["video_model"].(map[string]any)
	if encoded, ok := entry["video_model"].(string); ok && encoded != "" {
		if decoded, err := decodeHongguoBase64(encoded); err == nil && len(decoded) > 0 {
			encoded = string(decoded)
		}
		decoder := json.NewDecoder(strings.NewReader(encoded))
		decoder.UseNumber()
		if err := decoder.Decode(&model); err == nil {
			return model
		}
		return nil
	}
	if len(model) > 0 {
		return model
	}
	// 兜底：某些返回把 video_list 直接平铺在 video 节点上。
	if rows := anyList(entry["video_list"]); len(rows) > 0 {
		return map[string]any{
			"video_list":     entry["video_list"],
			"video_duration": entry["video_duration"],
			"duration":       entry["duration"],
		}
	}
	return nil
}

func selectHongguoAppMedia(model map[string]any) (providerMedia, error) {
	variants := anyList(model["video_list"])
	if rows, ok := model["video_list"].(map[string]any); ok && len(variants) == 0 {
		keys := make([]string, 0, len(rows))
		for key := range rows {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			variants = append(variants, rows[key])
		}
	}
	duration, _ := strconv.ParseFloat(mapString(model, "video_duration", "duration"), 64)
	var keyErr error
	type scoredMedia struct {
		media providerMedia
		score int
	}
	var choices []scoredMedia
	for _, row := range variants {
		variant, _ := row.(map[string]any)
		meta := nestedMap(variant, "video_meta")
		codec := strings.ToLower(mapString(meta, "codec_type"))
		if codec == "bytevc2" || strings.Contains(strings.ToLower(mapString(variant, "gear_des_key")), "bytevc2") {
			continue
		}
		addresses := hongguoMediaAddresses(variant)
		if len(addresses) == 0 {
			continue
		}
		media := providerMedia{Referer: "https://novel.snssdk.com/", Duration: time.Duration(duration * float64(time.Second))}
		encryption := nestedMap(variant, "encrypt_info")
		spade := mapString(encryption, "spade_a")
		if spade != "" || encryption["encrypt"] == true || mapString(encryption, "encryption_method") == "cenc-aes-ctr" {
			var err error
			media.CENCKey, err = hongguoContentKey(spade)
			if err != nil {
				keyErr = err
				continue
			}
		}
		height, _ := strconv.Atoi(mapString(meta, "vheight"))
		if definition, err := strconv.Atoi(hongguoQualityNumber.FindString(mapString(meta, "definition"))); err == nil && definition > 0 {
			height = definition
		} else if width, _ := strconv.Atoi(mapString(meta, "vwidth")); width > 0 && (height == 0 || width < height) {
			height = width
		}
		media.Quality = height
		quality := height * 10
		if codec == "h264" || codec == "avc1" {
			quality++
		}
		for _, address := range addresses {
			media.URL = address
			choices = append(choices, scoredMedia{media: media, score: quality})
		}
	}
	if len(choices) > 0 {
		sort.SliceStable(choices, func(i, j int) bool { return choices[i].score > choices[j].score })
		selected := choices[0].media
		for _, choice := range choices {
			selected.Variants = append(selected.Variants, choice.media)
		}
		return selected, nil
	}
	if keyErr != nil {
		return providerMedia{}, fmt.Errorf("红果 App 媒体密钥不可用: %w", keyErr)
	}
	return providerMedia{}, errors.New("红果 App 未返回兼容的媒体，已跳过不支持的编码")
}

func hongguoMediaAddresses(info map[string]any) []string {
	var addresses []string
	seen := map[string]bool{}
	var add func(any)
	add = func(value any) {
		switch value := value.(type) {
		case string:
			address := strings.TrimSpace(value)
			if len(address) > 8192 {
				return
			}
			if !isProviderHTTPMediaURL(address) {
				decoded, err := decodeHongguoBase64(address)
				if err != nil {
					return
				}
				address = strings.TrimSpace(string(decoded))
			}
			if isProviderHTTPMediaURL(address) && !seen[address] {
				seen[address] = true
				addresses = append(addresses, address)
			}
		case []any:
			for _, item := range value {
				add(item)
			}
		}
	}
	for _, key := range []string{"main_url", "backup_url", "backup_url_1", "backup_url_2", "backup_urls", "url_list"} {
		add(info[key])
	}
	return addresses
}
