package controller

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"one-api/common"
	"one-api/model"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func relayTextHelper(c *gin.Context, relayMode int) *OpenAIErrorWithStatusCode {
	channelType := c.GetInt("channel")
	tokenId := c.GetInt("token_id")
	userId := c.GetInt("id")
	consumeQuota := c.GetBool("consume_quota")
	group := c.GetString("group")
	var textRequest GeneralOpenAIRequest
	if consumeQuota || channelType == common.ChannelTypeAzure || channelType == common.ChannelTypePaLM {
		err := common.UnmarshalBodyReusable(c, &textRequest)
		if err != nil {
			return errorWrapper(err, "bind_request_body_failed", http.StatusBadRequest)
		}
	}
	if relayMode == RelayModeModerations && textRequest.Model == "" {
		textRequest.Model = "text-moderation-latest"
	}
	if relayMode == RelayModeEmbeddings && textRequest.Model == "" {
		textRequest.Model = c.Param("model")
	}
	// request validation
	if textRequest.Model == "" {
		return errorWrapper(errors.New("model is required"), "required_field_missing", http.StatusBadRequest)
	}
	switch relayMode {
	case RelayModeCompletions:
		if textRequest.Prompt == "" {
			return errorWrapper(errors.New("field prompt is required"), "required_field_missing", http.StatusBadRequest)
		}
	case RelayModeChatCompletions:
		if textRequest.Messages == nil || len(textRequest.Messages) == 0 {
			return errorWrapper(errors.New("field messages is required"), "required_field_missing", http.StatusBadRequest)
		}
	case RelayModeEmbeddings:
	case RelayModeModerations:
		if textRequest.Input == "" {
			return errorWrapper(errors.New("field input is required"), "required_field_missing", http.StatusBadRequest)
		}
	case RelayModeEdits:
		if textRequest.Instruction == "" {
			return errorWrapper(errors.New("field instruction is required"), "required_field_missing", http.StatusBadRequest)
		}
	}
	// map model name
	modelMapping := c.GetString("model_mapping")
	isModelMapped := false
	if modelMapping != "" {
		modelMap := make(map[string]string)
		err := json.Unmarshal([]byte(modelMapping), &modelMap)
		if err != nil {
			return errorWrapper(err, "unmarshal_model_mapping_failed", http.StatusInternalServerError)
		}
		if modelMap[textRequest.Model] != "" {
			textRequest.Model = modelMap[textRequest.Model]
			isModelMapped = true
		}
	}

	// Get token info
	tokenInfo, err := model.GetTokenById(tokenId)

	if err != nil {
		return errorWrapper(err, "get_token_info_failed", http.StatusInternalServerError)
	}

	hasModelAvailable := func() bool {
		for _, token := range strings.Split(tokenInfo.Models, ",") {
			if token == textRequest.Model {
				return true
			}
		}
		return false
	}()

	if !hasModelAvailable {
		return errorWrapper(errors.New("model not available for use"), "model_not_available_for_use", http.StatusBadRequest)
	}

	baseURL := common.ChannelBaseURLs[channelType]
	requestURL := c.Request.URL.String()
	if c.GetString("base_url") != "" {
		baseURL = c.GetString("base_url")
	}
	fullRequestURL := fmt.Sprintf("%s%s", baseURL, requestURL)
	if channelType == common.ChannelTypeAzure {
		// https://learn.microsoft.com/en-us/azure/cognitive-services/openai/chatgpt-quickstart?pivots=rest-api&tabs=command-line#rest-api
		query := c.Request.URL.Query()
		apiVersion := query.Get("api-version")
		if apiVersion == "" {
			apiVersion = c.GetString("api_version")
		}
		requestURL := strings.Split(requestURL, "?")[0]
		requestURL = fmt.Sprintf("%s?api-version=%s", requestURL, apiVersion)
		baseURL = c.GetString("base_url")
		task := strings.TrimPrefix(requestURL, "/v1/")
		model_ := textRequest.Model
		model_ = strings.Replace(model_, ".", "", -1)
		// https://github.com/songquanpeng/one-api/issues/67
		model_ = strings.TrimSuffix(model_, "-0301")
		model_ = strings.TrimSuffix(model_, "-0314")
		model_ = strings.TrimSuffix(model_, "-0613")
		fullRequestURL = fmt.Sprintf("%s/openai/deployments/%s/%s", baseURL, model_, task)
	} else if channelType == common.ChannelTypeChatGPTWeb {
		// remove /v1/chat/completions from request url
		requestURL := strings.Split(requestURL, "/v1/chat/completions")[0]
		fullRequestURL = fmt.Sprintf("%s%s", baseURL, requestURL)
	} else if channelType == common.ChannelTypeChatbotUI {
		// remove /v1/chat/completions from request url
		requestURL := strings.Split(requestURL, "/v1/chat/completions")[0]
		fullRequestURL = fmt.Sprintf("%s%s", baseURL, requestURL)
	} else if channelType == common.ChannelTypePaLM {
		err := relayPaLM(textRequest, c)
		return err
	}
	var promptTokens int
	var completionTokens int
	switch relayMode {
	case RelayModeChatCompletions:
		promptTokens = countTokenMessages(textRequest.Messages, textRequest.Model)
	case RelayModeCompletions:
		promptTokens = countTokenInput(textRequest.Prompt, textRequest.Model)
	case RelayModeModerations:
		promptTokens = countTokenInput(textRequest.Input, textRequest.Model)
	}
	preConsumedTokens := common.PreConsumedQuota
	if textRequest.MaxTokens != 0 {
		preConsumedTokens = promptTokens + textRequest.MaxTokens
	}
	modelRatio := common.GetModelRatio(textRequest.Model)
	groupRatio := common.GetGroupRatio(group)
	ratio := modelRatio * groupRatio
	preConsumedQuota := int(float64(preConsumedTokens) * ratio)
	userQuota, err := model.CacheGetUserQuota(userId)
	if err != nil {
		return errorWrapper(err, "get_user_quota_failed", http.StatusInternalServerError)
	}
	if userQuota > 10*preConsumedQuota {
		// in this case, we do not pre-consume quota
		// because the user has enough quota
		preConsumedQuota = 0
	}
	if consumeQuota && preConsumedQuota > 0 {
		err := model.PreConsumeTokenQuota(tokenId, preConsumedQuota)
		if err != nil {
			return errorWrapper(err, "pre_consume_token_quota_failed", http.StatusForbidden)
		}
	}
	var requestBody io.Reader
	if isModelMapped {
		jsonStr, err := json.Marshal(textRequest)
		if err != nil {
			return errorWrapper(err, "marshal_text_request_failed", http.StatusInternalServerError)
		}
		requestBody = bytes.NewBuffer(jsonStr)
	} else {
		bodyBytes, err := io.ReadAll(c.Request.Body)
		if err != nil {
			return errorWrapper(err, "read_request_body_failed", http.StatusInternalServerError)
		}
		var bodyMap map[string]interface{}
		err = json.Unmarshal(bodyBytes, &bodyMap)
		if err != nil {
			return errorWrapper(err, "unmarshal_request_body_failed", http.StatusInternalServerError)
		}

		// Add "stream":true to body map if it doesn't exist
		if _, exists := bodyMap["stream"]; !exists {
			bodyMap["stream"] = true
		}

		// Marshal the body map back into JSON
		bodyBytes, err = json.Marshal(bodyMap)
		if err != nil {
			return errorWrapper(err, "marshal_request_body_failed", http.StatusInternalServerError)
		}

		requestBody = bytes.NewBuffer(bodyBytes)
	}

	if channelType == common.ChannelTypeChatGPTWeb {
		// Get system message from Message json, Role == "system"
		var reqBody ChatRequest
		var systemMessage Message

		// Parse requestBody into systemMessage
		err := json.NewDecoder(requestBody).Decode(&reqBody)

		if err != nil {
			return errorWrapper(err, "decode_request_body_failed", http.StatusInternalServerError)
		}

		for _, message := range reqBody.Messages {
			if message.Role == "system" {
				systemMessage = message
				break
			}
		}

		var prompt string

		// Get all the Message, Roles from request.Messages, and format it into string by
		// ||> role: content
		for _, message := range reqBody.Messages {
			// Exclude system message
			if message.Role == "system" {
				continue
			}
			prompt += "||> " + message.Role + ": " + message.Content + "\n"
		}

		// Construct json data without adding escape character
		map1 := make(map[string]interface{})

		map1["prompt"] = prompt + "\nResponse as assistant, but do not include the role in response."
		map1["systemMessage"] = systemMessage.Content

		if reqBody.Temperature != 0 {
			map1["temperature"] = formatFloat(reqBody.Temperature)
		}
		if reqBody.TopP != 0 {
			map1["top_p"] = formatFloat(reqBody.TopP)
		}

		// Convert map to json string
		jsonData, err := json.Marshal(map1)

		if err != nil {
			return errorWrapper(err, "marshal_json_failed", http.StatusInternalServerError)
		}

		// Convert json string to io.Reader
		requestBody = bytes.NewReader(jsonData)
	} else if channelType == common.ChannelTypeChatbotUI {
		// Get system message from Message json, Role == "system"
		var reqBody ChatRequest

		// Parse requestBody into systemMessage
		err := json.NewDecoder(requestBody).Decode(&reqBody)

		if err != nil {
			return errorWrapper(err, "decode_request_body_failed", http.StatusInternalServerError)
		}

		// Get system message from Message json, Role == "system"
		var systemMessage string

		for _, message := range reqBody.Messages {
			if message.Role == "system" {
				systemMessage = message.Content
				break
			}
		}

		// Construct json data without adding escape character
		map1 := make(map[string]interface{})

		map1["prompt"] = systemMessage
		map1["temperature"] = formatFloat(reqBody.Temperature)
		map1["key"] = ""
		map1["messages"] = reqBody.Messages
		map1["model"] = map[string]interface{}{
			"id": reqBody.Model,
		}

		// Convert map to json string
		jsonData, err := json.Marshal(map1)

		if err != nil {
			return errorWrapper(err, "marshal_json_failed", http.StatusInternalServerError)
		}

		// Convert json string to io.Reader
		requestBody = bytes.NewReader(jsonData)
	}

	req, err := http.NewRequest(c.Request.Method, fullRequestURL, requestBody)
	if err != nil {
		return errorWrapper(err, "new_request_failed", http.StatusInternalServerError)
	}
	if channelType == common.ChannelTypeAzure {
		key := c.Request.Header.Get("Authorization")
		key = strings.TrimPrefix(key, "Bearer ")
		req.Header.Set("api-key", key)
	} else {
		req.Header.Set("Authorization", c.Request.Header.Get("Authorization"))
	}
	req.Header.Set("Content-Type", c.Request.Header.Get("Content-Type"))
	req.Header.Set("Accept", c.Request.Header.Get("Accept"))
	//req.Header.Set("Connection", c.Request.Header.Get("Connection"))

	if c.GetBool("enable_ip_randomization") == true {
		// Generate random IP
		ip := common.GenerateIP()
		req.Header.Set("X-Forwarded-For", ip)
		req.Header.Set("X-Real-IP", ip)
		req.Header.Set("X-Client-IP", ip)
		req.Header.Set("X-Forwarded-Host", ip)
		req.Header.Set("X-Originating-IP", ip)
		req.RemoteAddr = ip
		req.Header.Set("X-Remote-IP", ip)
		req.Header.Set("X-Remote-Addr", ip)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return errorWrapper(err, "do_request_failed", http.StatusInternalServerError)
	}
	if resp.StatusCode != http.StatusOK {
		// Print the body in string
		if resp.Body != nil {
			buf := new(bytes.Buffer)
			buf.ReadFrom(resp.Body)
			log.Printf("Error Channel (%s): %s", baseURL, buf.String())
			return errorWrapper(err, "request_failed", resp.StatusCode)
		}

		return errorWrapper(err, "request_failed", resp.StatusCode)
	}
	err = req.Body.Close()
	if err != nil {
		return errorWrapper(err, "close_request_body_failed", http.StatusInternalServerError)
	}
	err = c.Request.Body.Close()
	if err != nil {
		return errorWrapper(err, "close_request_body_failed", http.StatusInternalServerError)
	}
	var textResponse TextResponse
	isStream := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") || strings.HasPrefix(resp.Header.Get("Content-Type"), "application/octet-stream")
	var streamResponseText string

	defer func() {
		if consumeQuota {
			quota := 0
			completionRatio := 1.0
			if strings.HasPrefix(textRequest.Model, "gpt-3.5") {
				completionRatio = 1.333333
			}
			if strings.HasPrefix(textRequest.Model, "gpt-4") {
				completionRatio = 2
			}
			if isStream {
				completionTokens = countTokenText(streamResponseText, textRequest.Model)
			} else {
				promptTokens = textResponse.Usage.PromptTokens
				completionTokens = textResponse.Usage.CompletionTokens
			}
			quota = promptTokens + int(float64(completionTokens)*completionRatio)
			quota = int(float64(quota) * ratio)
			if ratio != 0 && quota <= 0 {
				quota = 1
			}
			totalTokens := promptTokens + completionTokens
			if totalTokens == 0 {
				// in this case, must be some error happened
				// we cannot just return, because we may have to return the pre-consumed quota
				quota = 0
			}
			quotaDelta := quota - preConsumedQuota
			err := model.PostConsumeTokenQuota(tokenId, quotaDelta)
			if err != nil {
				common.SysError("error consuming token remain quota: " + err.Error())
			}
			err = model.CacheUpdateUserQuota(userId)
			if err != nil {
				common.SysError("error update user quota cache: " + err.Error())
			}
			if quota != 0 {
				tokenName := c.GetString("token_name")
				logContent := fmt.Sprintf("模型倍率 %.2f，分组倍率 %.2f", modelRatio, groupRatio)
				model.RecordConsumeLog(userId, promptTokens, completionTokens, textRequest.Model, tokenName, quota, logContent)
				model.UpdateUserUsedQuotaAndRequestCount(userId, quota)
				channelId := c.GetInt("channel_id")
				model.UpdateChannelUsedQuota(channelId, quota)
			}
		}
	}()

	if isStream || channelType == common.ChannelTypeChatGPTWeb || channelType == common.ChannelTypeChatbotUI {
		dataChan := make(chan string)
		stopChan := make(chan bool)

		scanner := bufio.NewScanner(resp.Body)

		if channelType != common.ChannelTypeChatGPTWeb && channelType != common.ChannelTypeChatbotUI {
			scanner.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
				if atEOF && len(data) == 0 {
					return 0, nil, nil
				}

				if i := strings.Index(string(data), "\n"); i >= 0 {
					return i + 2, data[0:i], nil
				}

				if atEOF {
					return len(data), data, nil
				}

				return 0, nil, nil
			})
		}

		go func() {
			for scanner.Scan() {
				data := scanner.Text()
				if len(data) < 6 { // must be something wrong!
					continue
				}

				if channelType == common.ChannelTypeChatGPTWeb {
					var chatResponse ChatGptWebChatResponse
					err = json.Unmarshal([]byte(data), &chatResponse)
					if err != nil {
						// Print the body in string
						buf := new(bytes.Buffer)
						buf.ReadFrom(resp.Body)
						common.SysError("error unmarshalling chat response: " + err.Error() + " " + buf.String())
						return
					}

					// if response role is assistant and contains delta, append the content to streamResponseText
					if chatResponse.Role == "assistant" && chatResponse.Detail != nil {
						for _, choice := range chatResponse.Detail.Choices {
							streamResponseText += choice.Delta.Content

							returnObj := map[string]interface{}{
								"id":      chatResponse.ID,
								"object":  chatResponse.Detail.Object,
								"created": chatResponse.Detail.Created,
								"model":   chatResponse.Detail.Model,
								"choices": []map[string]interface{}{
									// set finish_reason to null in json
									{
										"finish_reason": nil,
										"index":         0,
										"delta": map[string]interface{}{
											"content": choice.Delta.Content,
										},
									},
								},
							}

							jsonData, _ := json.Marshal(returnObj)

							dataChan <- "data: " + string(jsonData)
						}
					}
				} else if channelType == common.ChannelTypeChatbotUI {
					returnObj := map[string]interface{}{
						"id":      "chatcmpl-" + strconv.Itoa(int(time.Now().UnixNano())),
						"object":  "text_completion",
						"created": time.Now().Unix(),
						"model":   textRequest.Model,
						"choices": []map[string]interface{}{
							// set finish_reason to null in json
							{
								"finish_reason": nil,
								"index":         0,
								"delta": map[string]interface{}{
									"content": data,
								},
							},
						},
					}

					jsonData, _ := json.Marshal(returnObj)

					dataChan <- "data: " + string(jsonData)
				} else {
					// If data has event: event content inside, remove it, it can be prefix or inside the data
					if strings.HasPrefix(data, "event:") || strings.Contains(data, "event:") {
						// Remove event: event in the front or back
						data = strings.TrimPrefix(data, "event: event")
						data = strings.TrimSuffix(data, "event: event")
						// Remove everything, only keep `data: {...}` <--- this is the json
						// Find the start and end indices of `data: {...}` substring
						startIndex := strings.Index(data, "data:")
						endIndex := strings.LastIndex(data, "}")

						// If both indices are found and end index is greater than start index
						if startIndex != -1 && endIndex != -1 && endIndex > startIndex {
							// Extract the `data: {...}` substring
							data = data[startIndex : endIndex+1]
						}

						// Trim whitespace and newlines from the modified data string
						data = strings.TrimSpace(data)
					}
					if !strings.HasPrefix(data, "data:") {
						continue
					}
					dataChan <- data
					data = data[6:]
					if !strings.HasPrefix(data, "[DONE]") {
						switch relayMode {
						case RelayModeChatCompletions:
							var streamResponse ChatCompletionsStreamResponse
							err = json.Unmarshal([]byte(data), &streamResponse)
							if err != nil {
								common.SysError("error unmarshalling stream response: " + err.Error())
								return
							}
							for _, choice := range streamResponse.Choices {
								streamResponseText += choice.Delta.Content
							}
						case RelayModeCompletions:
							var streamResponse CompletionsStreamResponse
							err = json.Unmarshal([]byte(data), &streamResponse)
							if err != nil {
								common.SysError("error unmarshalling stream response: " + err.Error())
								return
							}
							for _, choice := range streamResponse.Choices {
								streamResponseText += choice.Text
							}
						}

					}

				}
			}
			stopChan <- true
		}()

		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.Header().Set("Transfer-Encoding", "chunked")
		c.Writer.Header().Set("X-Accel-Buffering", "no")
		c.Stream(func(w io.Writer) bool {
			select {
			case data := <-dataChan:
				if strings.HasPrefix(data, "data: [DONE]") {
					data = data[:12]
				}
				// some implementations may add \r at the end of data
				data = strings.TrimSuffix(data, "\r")
				c.Render(-1, common.CustomEvent{Data: data})
				return true
			case <-stopChan:
				return false
			}
		})
		err = resp.Body.Close()
		if err != nil {
			return errorWrapper(err, "close_response_body_failed", http.StatusInternalServerError)
		}
		return nil
	} else {
		if consumeQuota {
			responseBody, err := io.ReadAll(resp.Body)
			if err != nil {
				return errorWrapper(err, "read_response_body_failed", http.StatusInternalServerError)
			}
			err = resp.Body.Close()
			if err != nil {
				return errorWrapper(err, "close_response_body_failed", http.StatusInternalServerError)
			}
			err = json.Unmarshal(responseBody, &textResponse)
			if err != nil {
				return errorWrapper(err, "unmarshal_response_body_failed", http.StatusInternalServerError)
			}
			if textResponse.Error.Type != "" {
				return &OpenAIErrorWithStatusCode{
					OpenAIError: textResponse.Error,
					StatusCode:  resp.StatusCode,
				}
			}
			// Reset response body
			resp.Body = io.NopCloser(bytes.NewBuffer(responseBody))
		}
		// We shouldn't set the header before we parse the response body, because the parse part may fail.
		// And then we will have to send an error response, but in this case, the header has already been set.
		// So the client will be confused by the response.
		// For example, Postman will report error, and we cannot check the response at all.
		for k, v := range resp.Header {
			c.Writer.Header().Set(k, v[0])
		}
		c.Writer.WriteHeader(resp.StatusCode)
		_, err = io.Copy(c.Writer, resp.Body)
		if err != nil {
			return errorWrapper(err, "copy_response_body_failed", http.StatusInternalServerError)
		}
		err = resp.Body.Close()
		if err != nil {
			return errorWrapper(err, "close_response_body_failed", http.StatusInternalServerError)
		}
		return nil
	}
}
