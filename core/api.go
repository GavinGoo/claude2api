package core

import (
	"bufio"
	"claude2api/logger"
	"claude2api/config"
	"claude2api/model"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/imroc/req/v3"
)

type Client struct {
	SessionKey   string
	orgID        string
	cookie       string
	client       *req.Client
	model        string
	thinking	 string
	defaultAttrs map[string]interface{}
}

type ResponseEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Delta struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		THINKING string `json:"thinking"`
		// partial_json
		PartialJSON string `json:"partial_json"`
	} `json:"delta"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

func NewClient(sessionKey string, proxy string, model string, thinking string, cookie string) *Client {
	client := req.C().
				ImpersonateChrome().
				SetTimeout(time.Minute * 5).
				// EnableForceHTTP2().
				SetUserAgent("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36").
				SetTLSFingerprintEdge().
				DevMode()
	client.Transport.SetResponseHeaderTimeout(time.Second * 10)
	if proxy != "" {
		client.SetProxyURL(proxy)
	}
	// Set common headers
	headers := map[string]string{
		"accept":                    "text/event-stream, text/event-stream",
		"accept-language":           "zh-CN,zh;q=0.9",
		"anthropic-client-platform": "web_claude_ai",
		"content-type":              "application/json",
		"origin":                    config.ConfigInstance.MirrorProxy,
		"priority":                  "u=1, i",
	}
	for key, value := range headers {
		client.SetCommonHeader(key, value)
	}
	// Set cookies
	client.SetCommonCookies(&http.Cookie{
		Name:  "sessionKey",
		Value: sessionKey,
	})
	if config.ConfigInstance.FuClaude {
		// logger.Info("FuClaude enabled, setting _Secure-next-auth.session-data cookie: %s", cookie)
		client.SetCommonCookies(&http.Cookie{
			Name:  "_Secure-next-auth.session-data",
			Value: cookie,
		})
	}
	// Create default client with session key
	c := &Client{
		SessionKey: sessionKey,
		client:     client,
		model:      model,
		thinking:   thinking,
		defaultAttrs: map[string]interface{}{
			"personalized_styles": []map[string]interface{}{
				{
					"type":       "default",
					"key":        "Default",
					"name":       "Normal",
					"nameKey":    "normal_style_name",
					"prompt":     "Normal",
					"summary":    "Default responses from Claude",
					"summaryKey": "normal_style_summary",
					"isDefault":  true,
				},
			},
			"tools": []map[string]interface{}{
				{
					"type": "web_search_v0",
					"name": "web_search",
				},
				{"type": "artifacts_v0", "name": "artifacts"},
				{"type": "repl_v0", "name": "repl"},
			},
			"parent_message_uuid": "00000000-0000-4000-8000-000000000000",
			"attachments":         []interface{}{},
			"files":               []interface{}{},
			"sync_sources":        []interface{}{},
			"locale":              "en-US",
			"rendering_mode":      "messages",
			"timezone":            "America/New_York",
			// "timezone":            "Asia/Shanghai",
		},
	}
	return c
}

// SetOrgID sets the organization ID for the client
func (c *Client) SetOrgID(orgID string) {
	c.orgID = orgID
}
func (c *Client) GetOrgID() (string, error) {
	url := config.ConfigInstance.MirrorProxy+"/api/organizations"
	logger.Info("Handling GetOrgID_url: " + url)
	resp, err := c.client.R().
		SetHeader("referer", config.ConfigInstance.MirrorProxy+"/new").
		Get(url)
	logger.Info("Handling GetOrgID: " + resp.String())
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	type OrgResponse []struct {
		ID            int    `json:"id"`
		UUID          string `json:"uuid"`
		Name          string `json:"name"`
		RateLimitTier string `json:"rate_limit_tier"`
	}

	var orgs OrgResponse
	if err := json.Unmarshal(resp.Bytes(), &orgs); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}
	if len(orgs) == 0 {
		return "", errors.New("no organizations found")
	}
	if len(orgs) == 1 {
		return orgs[0].UUID, nil
	}
	for _, org := range orgs {
		if org.RateLimitTier == "default_claude_ai" {
			return org.UUID, nil
		}
	}
	return "", errors.New("no default organization found")

}

// CreateConversation creates a new conversation and returns its UUID
func (c *Client) CreateConversation() (string, error) {
	if c.orgID == "" {
		return "", errors.New("organization ID not set")
	}
	logger.Info("Handling CreateConversation: " + c.orgID)
	url := fmt.Sprintf(config.ConfigInstance.MirrorProxy+"/api/organizations/%s/chat_conversations", c.orgID)
	// 如果以-think结尾
	if strings.HasSuffix(c.model, "-think") {
		c.model = strings.TrimSuffix(c.model, "-think")
		if c.thinking == "null" || c.thinking == "false" {
			if err := c.UpdateUserSetting("paprika_mode", "extended"); err != nil {
				logger.Error(fmt.Sprintf("Failed to update paprika_mode: %v", err))
			}
		}
	} else {
		if c.thinking == "null" || c.thinking == "true" {
			if err := c.UpdateUserSetting("paprika_mode", nil); err != nil {
				logger.Error(fmt.Sprintf("Failed to update paprika_mode: %v", err))
			}
		}
	}
	requestBody := map[string]interface{}{
		"model":                            c.model,
		"uuid":                             uuid.New().String(),
		"name":                             "",
		"include_conversation_preferences": true,
	}
	if c.model == "claude-sonnet-4-20250514" {
		// 删除model
		delete(requestBody, "model")
	}

	resp, err := c.client.R().
		SetHeader("referer", config.ConfigInstance.MirrorProxy+"/new").
		SetBody(requestBody).
		Post(url)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	var result map[string]interface{}
	// logger.Info(fmt.Sprintf("create conversation response: %s", resp.String()))
	if err := json.Unmarshal(resp.Bytes(), &result); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	uuid, ok := result["uuid"].(string)
	if !ok {
		return "", errors.New("conversation UUID not found in response")
	}
	return uuid, nil
}

// SendMessage sends a message to a conversation and returns the status and response
func (c *Client) SendMessage(conversationID string, message string, stream bool, gc *gin.Context) (int, error) {
	logger.Info("Handling message for conversation: " + conversationID)
	if c.orgID == "" {
		return 500, errors.New("organization ID not set")
	}
	url := fmt.Sprintf(config.ConfigInstance.MirrorProxy+"/api/organizations/%s/chat_conversations/%s/completion",
		c.orgID, conversationID)
	// Create request body with default attributes
	requestBody := c.defaultAttrs
	requestBody["prompt"] = message
	if c.model != "claude-sonnet-4-20250514" {
		requestBody["model"] = c.model
	}
	// Set up streaming response
	resp, err := c.client.R().DisableAutoReadResponse().
		SetHeader("referer", fmt.Sprintf(config.ConfigInstance.MirrorProxy+"/chat/%s", conversationID)).
		SetHeader("accept", "text/event-stream, text/event-stream").
		SetHeader("anthropic-client-platform", "web_claude_ai").
		SetHeader("cache-control", "no-cache").
		SetBody(requestBody).
		Post(url)
	if err != nil {
		return 500, fmt.Errorf("request failed: %w", err)
	}
	logger.Info(fmt.Sprintf("Claude response status code: %d", resp.StatusCode))
	if resp.StatusCode == http.StatusTooManyRequests {
		return http.StatusTooManyRequests, fmt.Errorf("rate limit exceeded")
	}
	if resp.StatusCode != http.StatusOK {
		logger.Error("Handling SendMessage Error: " + resp.String())
		return resp.StatusCode, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	return 200, c.HandleResponse(resp.Body, stream, gc)
}

// HandleResponse converts Claude's SSE format to OpenAI format and writes to the response writer
func (c *Client) HandleResponse(body io.ReadCloser, stream bool, gc *gin.Context) error {
	defer body.Close()
	// Set headers for streaming
	if stream {
		gc.Writer.Header().Set("Content-Type", "text/event-stream")
		gc.Writer.Header().Set("Cache-Control", "no-cache")
		gc.Writer.Header().Set("Connection", "keep-alive")
		// 发送200状态码
		gc.Writer.WriteHeader(http.StatusOK)
		gc.Writer.Flush()
	}
	scanner := bufio.NewScanner(body)
	clientDone := gc.Request.Context().Done()
	// Keep track of the full response for the final message
	thinkingShown := false
	res_all_text := ""
	partial_json_shown := false
	for scanner.Scan() {
		select {
		case <-clientDone:
			// 客户端已断开连接，清理资源并退出
			logger.Info("Client closed connection")
			return nil
		default:
			// 继续处理响应
		}
		line := scanner.Text()
		logger.Info(fmt.Sprintf("Claude SSE line: %s", line))
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := line[6:]
		var event ResponseEvent
		if err := json.Unmarshal([]byte(data), &event); err == nil {
			if event.Type == "error" && event.Error.Message != "" {
				model.ReturnOpenAIResponse(event.Error.Message, stream, gc)
				return nil
			}
			if event.Delta.Type == "text_delta" && event.Delta.Text != "" {
				res_text := event.Delta.Text
				if thinkingShown {
					res_text = "</think>\n" + res_text
					thinkingShown = false
				}
				if partial_json_shown {
					res_text = "\n```\n" + res_text
					partial_json_shown = false
				}
				res_all_text += res_text
				if !stream {
					continue
				}
				model.ReturnOpenAIResponse(res_text, stream, gc)
				continue
			}
			if event.Delta.Type == "thinking_delta" {
				res_text := event.Delta.THINKING
				if !thinkingShown {
					res_text = "<think> " + res_text
					thinkingShown = true
				}
				res_all_text += res_text
				if !stream {
					continue
				}
				model.ReturnOpenAIResponse(res_text, stream, gc)
				continue
			}
			if event.Delta.Type == "input_json_delta" {
				res_text := event.Delta.PartialJSON
				if !partial_json_shown {
					res_text = "\n```\n " + res_text
					partial_json_shown = true
				}
				res_all_text += res_text
				if !stream {
					continue
				}
				model.ReturnOpenAIResponse(res_text, stream, gc)
				continue
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("error reading response: %w", err)
	}
	if !stream {
		model.ReturnOpenAIResponse(res_all_text, stream, gc)
	} else {
		// 发送结束标志
		gc.Writer.Write([]byte("data: [DONE]\n\n"))
		gc.Writer.Flush()
	}

	return nil
}

// DeleteConversation deletes a conversation by ID
func (c *Client) DeleteConversation(conversationID string) error {
	if c.orgID == "" {
		return errors.New("organization ID not set")
	}
	url := fmt.Sprintf(config.ConfigInstance.MirrorProxy+"/api/organizations/%s/chat_conversations/%s",
		c.orgID, conversationID)
	requestBody := map[string]string{
		"uuid": conversationID,
	}
	resp, err := c.client.R().
		SetHeader("referer", fmt.Sprintf(config.ConfigInstance.MirrorProxy+"/chat/%s", conversationID)).
		SetBody(requestBody).
		Delete(url)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	return nil
}

// UploadFile uploads files to Claude and adds them to the client's default attributes
// fileData should be in the format: data:image/jpeg;base64,/9j/4AA...
func (c *Client) UploadFile(fileData []string) error {
	if c.orgID == "" {
		return errors.New("organization ID not set")
	}
	if len(fileData) == 0 {
		return errors.New("empty file data")
	}

	// Initialize files array in default attributes if it doesn't exist
	if _, ok := c.defaultAttrs["files"]; !ok {
		c.defaultAttrs["files"] = []interface{}{}
	}

	// Process each file
	for _, fd := range fileData {
		if fd == "" {
			continue // Skip empty entries
		}

		// Parse the base64 data
		parts := strings.SplitN(fd, ",", 2)
		if len(parts) != 2 {
			return errors.New("invalid file data format")
		}

		// Get the content type from the data URI
		metaParts := strings.SplitN(parts[0], ":", 2)
		if len(metaParts) != 2 {
			return errors.New("invalid content type in file data")
		}

		metaInfo := strings.SplitN(metaParts[1], ";", 2)
		if len(metaInfo) != 2 || metaInfo[1] != "base64" {
			return errors.New("invalid encoding in file data")
		}

		contentType := metaInfo[0]

		// Decode the base64 data
		fileBytes, err := base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return fmt.Errorf("failed to decode base64 data: %w", err)
		}

		// Determine filename based on content type
		var filename string
		switch contentType {
		case "image/jpeg":
			filename = "image.jpg"
		case "image/png":
			filename = "image.png"
		case "application/pdf":
			filename = "document.pdf"
		default:
			filename = "file"
		}

		// Create the upload URL
		url := fmt.Sprintf(config.ConfigInstance.MirrorProxy+"/api/%s/upload", c.orgID)

		// Create a multipart form request
		resp, err := c.client.R().
			SetHeader("referer", config.ConfigInstance.MirrorProxy+"/new").
			SetHeader("anthropic-client-platform", "web_claude_ai").
			SetFileBytes("file", filename, fileBytes).
			SetContentType("multipart/form-data").
			Post(url)

		if err != nil {
			return fmt.Errorf("request failed: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("unexpected status code: %d, response: %s", resp.StatusCode, resp.String())
		}

		// Parse the response
		var result struct {
			FileUUID string `json:"file_uuid"`
		}

		if err := json.Unmarshal(resp.Bytes(), &result); err != nil {
			return fmt.Errorf("failed to parse response: %w", err)
		}

		if result.FileUUID == "" {
			return errors.New("file UUID not found in response")
		}

		// Add file to default attributes
		c.defaultAttrs["files"] = append(c.defaultAttrs["files"].([]interface{}), result.FileUUID)
	}

	return nil
}

func (c *Client) SetBigContext(context string) {
	c.defaultAttrs["attachments"] = []map[string]interface{}{
		{
			"file_name":         "context.txt",
			"file_type":         "text/plain",
			"file_size":         len(context),
			"extracted_content": context,
		},
	}

}

// / UpdateUserSetting updates a single user setting on Claude.ai while preserving all other settings
func (c *Client) UpdateUserSetting(key string, value interface{}) error {
	url := config.ConfigInstance.MirrorProxy+"/api/account?statsig_hashing_algorithm=djb2"

	// Default settings structure with all possible fields
	settings := map[string]interface{}{
		"input_menu_pinned_items":          nil,
		"has_seen_mm_examples":             nil,
		"has_seen_starter_prompts":         nil,
		"has_started_claudeai_onboarding":  true,
		"has_finished_claudeai_onboarding": true,
		"dismissed_claudeai_banners":       []interface{}{},
		"dismissed_artifacts_announcement": nil,
		"preview_feature_uses_artifacts":   nil,
		"preview_feature_uses_latex":       nil,
		"preview_feature_uses_citations":   nil,
		"preview_feature_uses_harmony":     nil,
		"enabled_artifacts_attachments":    true,
		"enabled_turmeric":                 nil,
		"enable_chat_suggestions":          nil,
		"dismissed_artifact_feedback_form": nil,
		"enabled_mm_pdfs":                  nil,
		"enabled_gdrive":                   nil,
		"enabled_bananagrams":              nil,
		"enabled_gdrive_indexing":          nil,
		"enabled_web_search":               true,
		"enabled_compass":                  nil,
		"enabled_sourdough":                nil,
		"enabled_foccacia":                 nil,
		"dismissed_claude_code_spotlight":  nil,
		"enabled_geolocation":              nil,
		"enabled_mcp_tools":                nil,
		"paprika_mode":                     nil,
		"enabled_monkeys_in_a_barrel":      nil,
	}

	// Update the specified setting
	if _, exists := settings[key]; exists {
		settings[key] = value
		logger.Info(fmt.Sprintf("Updating setting %s to %v", key, value))
	} else {
		return fmt.Errorf("unknown setting key: %s", key)
	}

	// Create request body
	requestBody := map[string]interface{}{
		"settings": settings,
	}

	// Make the request
	resp, err := c.client.R().
		SetHeader("referer", config.ConfigInstance.MirrorProxy+"/new").
		SetHeader("origin", config.ConfigInstance.MirrorProxy).
		SetHeader("anthropic-client-platform", "web_claude_ai").
		SetHeader("cache-control", "no-cache").
		SetHeader("pragma", "no-cache").
		SetHeader("priority", "u=1, i").
		SetBody(requestBody).
		Put(url)

	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != 202 {
		return fmt.Errorf("unexpected status code: %d, response: %s", resp.StatusCode, resp.String())
	}

	// logger.Info(fmt.Sprintf("Successfully updated user setting %s: %s", key, resp.String()))
	return nil
}
