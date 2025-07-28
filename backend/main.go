// https://help.aliyun.com/zh/model-studio/qwen-tts?spm=a2c4g.11186623.0.0.123865c5uWt9vu
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"unicode"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
	"github.com/sashabaranov/go-openai"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type ChatSessionManager struct {
	sessions map[string]*ChatSession
	mu       sync.RWMutex
}

type ChatSession struct {
	sessionID       string
	client          *openai.Client
	model           string
	messages        []openai.ChatCompletionMessage
	textChunkQueue  chan TextChunk
	dashscopeApiKey string
	stopAudioSignal chan struct{}
}

type TextChunk struct {
	ID      string `json:"id"` // 为了该死的文本和音频同步
	Content string `json:"content"`
	URL     string `json:"url"` // 为了除了流式播放还可以听回放
}

func NewChatSessionManager() (*ChatSessionManager, error) {
	manager := &ChatSessionManager{
		sessions: make(map[string]*ChatSession),
	}
	return manager, nil
}

func (m *ChatSessionManager) NewChatSession(sessionID string) (*ChatSession, error) {
	_ = godotenv.Load()
	apiKey := os.Getenv("OPENAI_API_KEY")
	baseURL := os.Getenv("OPENAI_API_BASE")
	model := os.Getenv("OPENAI_API_MODEL")
	dashscopeApiKey := os.Getenv("DASHSCOPE_API_KEY")
	if apiKey == "" || baseURL == "" || model == "" || dashscopeApiKey == "" {
		return nil, errors.New("环境变量未设置")
	}

	config := openai.DefaultConfig(apiKey)
	config.BaseURL = baseURL
	client := openai.NewClientWithConfig(config)

	session := &ChatSession{
		sessionID:       sessionID,
		client:          client,
		model:           model,
		messages:        make([]openai.ChatCompletionMessage, 0),
		dashscopeApiKey: dashscopeApiKey,
		textChunkQueue:  make(chan TextChunk, 10000),
		stopAudioSignal: make(chan struct{}),
	}

	return session, nil
}

func (m *ChatSessionManager) AddSession(session *ChatSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[session.sessionID] = session
}

func (m *ChatSessionManager) GetSession(sessionID string) *ChatSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[sessionID]
}

func main() {
	sessionManager, err := NewChatSessionManager()
	if err != nil {
		log.Fatalf("创建会话管理器失败: %v", err)
	}

	session1, err := sessionManager.NewChatSession("sess1")
	if err != nil {
		log.Fatalf("创建会话失败: %v", err)
	}
	defer session1.StopAudioStream()
	//close(session1.textChunkQueue)

	session2, err := sessionManager.NewChatSession("sess2")
	if err != nil {
		log.Fatalf("创建会话失败: %v", err)
	}
	defer session2.StopAudioStream()
	//close(session2.textChunkQueue)

	sessionManager.AddSession(session1)
	sessionManager.AddSession(session2)

	http.HandleFunc("/ws/text-stream", TextStreamHandler(sessionManager))
	http.HandleFunc("/ws/audio-stream", AudioStreamHandler(sessionManager))
	http.HandleFunc("/ws/stop-audio-stream", StopAudioStreamHandler(sessionManager))

	log.Println("服务启动于 :8080")
	http.ListenAndServe(":8080", nil)
}

func TextStreamHandler(sessionManager *ChatSessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.URL.Query().Get("sessionid")
		if sessionID == "" {
			http.Error(w, "缺少 sessionid 参数", http.StatusBadRequest)
			return
		}

		session := sessionManager.GetSession(sessionID)
		if session == nil {
			http.Error(w, "无效的 sessionid", http.StatusBadRequest)
			return
		}

		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Println("升级websocket失败:", err)
			return
		}
		defer ws.Close()

		// System prompt 一次性调用
		// session.messages = append(session.messages, openai.ChatCompletionMessage{
		// 	Role:    openai.ChatMessageRoleSystem,
		// 	Content: "你是一个精打细算会过日子的家庭主妇",
		// })
		// content, err := session.CallOpenAI()
		// if err != nil {
		// 	log.Fatal(err)
		// }
		// session.messages = append(session.messages, openai.ChatCompletionMessage{
		// 	Role:    openai.ChatMessageRoleAssistant,
		// 	Content: content,
		// })

		// 后续对话用流
		session.processQuery(ws)
	}
}

func (session *ChatSession) processQuery(ws *websocket.Conn) {
	// 不断接收用户输入
	for {
		_, msgBytes, err := ws.ReadMessage()
		if err != nil {
			log.Printf("读取消息失败: %v", err)
			break
		}

		session.messages = append(session.messages, openai.ChatCompletionMessage{
			Role:    openai.ChatMessageRoleUser,
			Content: string(msgBytes),
		})

		messageID := uuid.New().String()

		var finalAnswer strings.Builder

		stream, err := session.client.CreateChatCompletionStream(context.Background(), openai.ChatCompletionRequest{
			Model:    session.model,
			Messages: session.messages,
			Stream:   true,
		})
		if err != nil {
			log.Printf("创建流失败: %v", err)
			return
		}

		for {
			resp, err := stream.Recv()

			if err != nil {
				textChunk := TextChunk{
					ID:      messageID,
					Content: "\n\n",
				}

				if errors.Is(err, io.EOF) {
					log.Println("stream finished")

					// 添加回答到历史消息
					session.messages = append(session.messages, openai.ChatCompletionMessage{
						Role:    openai.ChatMessageRoleAssistant,
						Content: finalAnswer.String(),
					})

					// 回答结速了告诉前端要换行
					ws.WriteJSON(textChunk)

					session.textChunkQueue <- textChunk
					break
				}

				log.Printf("接收流数据失败: %v", err)
				session.textChunkQueue <- textChunk
				stream.Close()
				return
			}

			choice := resp.Choices[0]
			content := choice.Delta.Content
			finalAnswer.WriteString(content)

			textChunk := TextChunk{
				ID:      messageID,
				Content: content,
			}
			session.textChunkQueue <- textChunk

			if err := ws.WriteJSON(textChunk); err != nil {
				log.Printf("websocket write error: %v", err)
				break
			}
		}

		stream.Close()
	}
}

func AudioStreamHandler(sessionManager *ChatSessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.URL.Query().Get("sessionid")
		if sessionID == "" {
			http.Error(w, "缺少 sessionid 参数", http.StatusBadRequest)
			return
		}

		session := sessionManager.GetSession(sessionID)
		if session == nil {
			http.Error(w, "无效的 sessionid", http.StatusBadRequest)
			return
		}

		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Fatal(err)
		}
		defer ws.Close()

		var segmentBuffer bytes.Buffer // 部分回答
		var wholeBuffer bytes.Buffer   // 完整回答

		for {
			select {
			case <-session.stopAudioSignal:
				log.Printf("音频流停止信号收到, 停止音频流处理")
				return
			case textChunk := <-session.textChunkQueue:
				log.Println(textChunk.Content)

				// 完整的回答结束了调用一次tts
				if textChunk.Content == "\n\n" {
					if segmentBuffer.Len() > 0 {
						session.processTTS(ws, filter(segmentBuffer.String()), textChunk.ID, true)
						segmentBuffer.Reset()
					}

					if wholeBuffer.Len() > 0 {
						session.processTTS(ws, filter(wholeBuffer.String()), textChunk.ID, false)
						wholeBuffer.Reset()
					}

					continue
				}

				// 每凑够100字节调用一次tts
				if segmentBuffer.Len() > 100 {
					session.processTTS(ws, filter(segmentBuffer.String()), textChunk.ID, true)
					segmentBuffer.Reset()
				}

				if textChunk.Content != "" {
					segmentBuffer.WriteString(textChunk.Content)
					wholeBuffer.WriteString(textChunk.Content)
				}
			}
		}
	}
}

type TTSRequest struct {
	Model string `json:"model"`
	Input struct {
		Text  string `json:"text"`
		Voice string `json:"voice"`
	} `json:"input"`
}

type TTSResponseChunk struct {
	Output struct {
		FinishReason string `json:"finish_reason"`
		Audio        struct {
			Data      string `json:"data"`
			URL       string `json:"url"`
			ExpiresAt int64  `json:"expires_at"`
			ID        string `json:"id"`
		} `json:"audio"`
	} `json:"output"`
}

// 请求
// curl -X POST 'https://dashscope.aliyuncs.com/api/v1/services/aigc/multimodal-generation/generation' \
// -H "Authorization: Bearer $DASHSCOPE_API_KEY" \
// -H 'Content-Type: application/json' \
// -H 'X-DashScope-SSE: enable' \
//
//	-d '{
//	    "model": "qwen-tts",
//	    "input": {
//	        "text": "那我来给大家推荐一款T恤，这款呢真的是超级好看，这个颜色呢很显气质，而且呢也是搭配的绝佳单品，大家可以闭眼入，真的是非常好看，对身材的包容性也很好，不管啥身材的宝宝呢，穿上去都是很好看的。推荐宝宝们下单哦。",
//	        "voice": "Chelsie"
//	    }
//	}'
//

// 响应
// id:1
// event:result
// :HTTP_STATUS/200
// data:{"output":{"finish_reason":"null","audio":{"data":"xxx","expires_at":1753403234,"id":"audio_2bf82975-d261-947a-9906-552ca8a647e9"}},"usage":{"total_tokens":65,"input_tokens_details":{"text_tokens":17},"output_tokens":48,"input_tokens":17,"output_tokens_details":{"audio_tokens":48,"text_tokens":0}},"request_id":"2bf82975-d261-947a-9906-552ca8a647e9"}

// id:2
// event:result
// :HTTP_STATUS/200
// data:{"output":{"finish_reason":"null","audio":{"data":"xxx","expires_at":1753403234,"id":"audio_2bf82975-d261-947a-9906-552ca8a647e9"}},"usage":{"total_tokens":101,"input_tokens_details":{"text_tokens":17},"output_tokens":84,"input_tokens":17,"output_tokens_details":{"audio_tokens":84,"text_tokens":0}},"request_id":"2bf82975-d261-947a-9906-552ca8a647e9"}

// id:3
// event:result
// :HTTP_STATUS/200
// data:{"output":{"finish_reason":"null","audio":{"data":"xxx","expires_at":1753403235,"id":"audio_2bf82975-d261-947a-9906-552ca8a647e9"}},"usage":{"total_tokens":137,"input_tokens_details":{"text_tokens":17},"output_tokens":120,"input_tokens":17,"output_tokens_details":{"audio_tokens":120,"text_tokens":0}},"request_id":"2bf82975-d261-947a-9906-552ca8a647e9"}

// id:4
// event:result
// :HTTP_STATUS/200
// data:{"output":{"finish_reason":"null","audio":{"data":"xxx","expires_at":1753403235,"id":"audio_2bf82975-d261-947a-9906-552ca8a647e9"}},"usage":{"total_tokens":152,"input_tokens_details":{"text_tokens":17},"output_tokens":135,"input_tokens":17,"output_tokens_details":{"audio_tokens":135,"text_tokens":0}},"request_id":"2bf82975-d261-947a-9906-552ca8a647e9"}

// id:5
// event:result
// :HTTP_STATUS/200
// data:{"output":{"finish_reason":"stop","audio":{"expires_at":1753489635,"id":"audio_2bf82975-d261-947a-9906-552ca8a647e9","data":"","url":"http://dashscope-result-wlcb.oss-cn-wulanchabu.aliyuncs.com/1d/1d/20250725/e6c1b9cc/ff3b8607-be6b-4260-aadd-47a91dd52f31.wav?Expires=1753489635&OSSAccessKeyId=LTAI5tKPD3TMqf2Lna1fASuh&Signature=LdJSRJ%2BKA6mNkBq3tPLfQNBnnMg%3D"}},"usage":{"total_tokens":152,"input_tokens_details":{"text_tokens":17},"output_tokens":135,"input_tokens":17,"output_tokens_details":{"audio_tokens":135,"text_tokens":0}},"request_id":"2bf82975-d261-947a-9906-552ca8a647e9"}
func (session *ChatSession) processTTS(ws *websocket.Conn, text string, messageID string, isSegment bool) error {
	ttsReq := TTSRequest{
		Model: "qwen-tts-latest",
	}
	ttsReq.Input.Text = text
	ttsReq.Input.Voice = "Chelsie"

	reqBodyBytes, err := json.Marshal(ttsReq)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", "https://dashscope.aliyuncs.com/api/v1/services/aigc/multimodal-generation/generation", strings.NewReader(string(reqBodyBytes)))
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+session.dashscopeApiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-DashScope-SSE", "enable")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				// SSE流结束
				break
			}
			return err
		}

		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ":") {
			// 空行或注释，跳过
			continue
		}

		// 解析 data 行
		if after, ok := strings.CutPrefix(line, "data:"); ok {
			var chunk TTSResponseChunk
			if err := json.Unmarshal([]byte(after), &chunk); err != nil {
				log.Println("解析TTS SSE失败:", err)
				continue
			}

			if isSegment {
				// 如果包含 base64 音频数据，解码并发送
				if chunk.Output.Audio.Data != "" {
					audioBytes, err := base64.StdEncoding.DecodeString(chunk.Output.Audio.Data)
					if err != nil {
						log.Println("base64解码失败:", err)
						continue
					}

					if err := ws.WriteMessage(websocket.BinaryMessage, audioBytes); err != nil {
						return err
					}
				}
			}

			if chunk.Output.FinishReason == "stop" && chunk.Output.Audio.URL != "" {
				if err := ws.WriteJSON(map[string]any{
					"messageID": messageID,
					"audioUrl":  chunk.Output.Audio.URL,
					"isSegment": isSegment,
				}); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (session *ChatSession) StopAudioStream() {
	close(session.stopAudioSignal)
}

func StopAudioStreamHandler(sessionManager *ChatSessionManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.URL.Query().Get("sessionid")
		if sessionID == "" {
			http.Error(w, "缺少 sessionid 参数", http.StatusBadRequest)
			return
		}

		session := sessionManager.GetSession(sessionID)
		if session == nil {
			http.Error(w, "无效的 sessionid", http.StatusBadRequest)
			return
		}

		// 调用 session.stopAudioStream() 来停止音频流的处理
		session.StopAudioStream()
		log.Printf("音频流已停止, 会话 %s", sessionID)

		w.Write([]byte("音频流已停止"))
	}
}

func removeEmoji(text string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case (r >= 0x1F600 && r <= 0x1F64F), // Emoticons
			(r >= 0x1F300 && r <= 0x1F5FF), // Misc Symbols and Pictographs
			(r >= 0x1F680 && r <= 0x1F6FF), // Transport and Map
			(r >= 0x1F900 && r <= 0x1F9FF), // Supplemental Symbols and Pictographs
			(r >= 0x1F1E6 && r <= 0x1F1FF), // Regional Indicator Symbols (flags)
			(r >= 0x2600 && r <= 0x27BF),   // Misc Symbols + Dingbats
			(r >= 0x1F3FB && r <= 0x1F3FF), // Skin tone modifiers
			r == 0x200D,                    // Zero Width Joiner
			r == 0xFE0F:                    // Variation Selector-16
			return -1
		default:
			return r
		}
	}, text)
}

func removePunctuation(text string) string {
	var builder strings.Builder
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsSpace(r) {
			builder.WriteRune(r)
		}
		// 标点符号及其他字符全部忽略
	}
	return builder.String()
}

func filter(text string) string {
	text = removeEmoji(text)
	text = removePunctuation(text)
	return text
}

func (session *ChatSession) CallOpenAI() (string, error) {
	resp, err := session.client.CreateChatCompletion(context.Background(), openai.ChatCompletionRequest{
		Model:    session.model,
		Messages: session.messages,
	})
	if err != nil {
		return "", err
	}
	return resp.Choices[0].Message.Content, nil
}
