package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/gotify/plugin-api"
)

// GetGotifyPluginInfo returns gotify plugin info.
func GetGotifyPluginInfo() plugin.Info {
	return plugin.Info{
		ModulePath:  "github.com/p3ddd/gotify-bark",
		Version:     "0.2.0",
		Author:      "Petrichor",
		Website:     "https://github.com/p3ddd/gotify-bark",
		License:     "MIT",
		Description: "Forwards Gotify messages to Bark by acting as a WebSocket client.",
		Name:        "Bark Forwarder",
	}
}

// BarkForwardPlugin is the gotify plugin instance for forwarding messages to Bark.
type BarkForwardPlugin struct {
	// done channel is used to signal the goroutine to stop
	done chan struct{}
	// config holds the user-provided configuration
	config *Config
	// httpClient is a shared HTTP client with timeout for Bark requests
	httpClient *http.Client
}

// Config defines the plugin config scheme.
type Config struct {
	// The WebSocket URL of the Gotify server, e.g., "ws://localhost:80"
	GotifyHost string `yaml:"gotify_host"`
	// A client token from Gotify for the plugin to use
	GotifyClientToken string `yaml:"gotify_client_token"`
	// The device key for your Bark account
	BarkDeviceKey string `yaml:"bark_device_key"`
	// The Bark server push URL
	BarkURL string `yaml:"bark_url"`
	// ReconnectDelay is the delay in seconds before trying to reconnect.
	ReconnectDelay int `yaml:"reconnect_delay,omitempty"`
	// EncryptionKey is the AES encryption key (16 bytes for AES-128, 32 bytes for AES-256). Leave empty to disable encryption.
	EncryptionKey string `yaml:"encryption_key,omitempty"`
	// EncryptionIV is the AES CBC initialization vector (must be 16 bytes). Leave empty to disable encryption.
	EncryptionIV string `yaml:"encryption_iv,omitempty"`
}

// DefaultConfig implements plugin.Configurer.
func (c *BarkForwardPlugin) DefaultConfig() any {
	return &Config{
		GotifyHost:        "ws://gotify:80",
		GotifyClientToken: "",
		BarkDeviceKey:     "",
		BarkURL:           "http://bark-server:8080/push",
		ReconnectDelay:    10,
	}
}

// ValidateAndSetConfig implements plugin.Configurer.
func (c *BarkForwardPlugin) ValidateAndSetConfig(config any) error {
	newConfig := config.(*Config)

	if newConfig.GotifyHost == "" {
		return errors.New("config: GotifyHost cannot be empty")
	}
	if newConfig.GotifyClientToken == "" {
		return errors.New("config: GotifyClientToken cannot be empty. Create a client in Gotify for this plugin")
	}
	if newConfig.BarkDeviceKey == "" {
		return errors.New("config: BarkDeviceKey key cannot be empty")
	}
	if newConfig.BarkURL == "" {
		return errors.New("config: BarkURL cannot be empty")
	}
	if newConfig.ReconnectDelay <= 0 {
		newConfig.ReconnectDelay = 10
	}

	c.config = newConfig
	log.Println("Bark Forwarder plugin configuration updated and validated.")
	return nil
}

// Enable enables the plugin.
func (c *BarkForwardPlugin) Enable() error {
	if c.config == nil {
		return errors.New("plugin is not configured yet")
	}

	// Initialize the done channel
	c.done = make(chan struct{})

	// Initialize HTTP client with timeout
	c.httpClient = &http.Client{
		Timeout: 30 * time.Second,
	}

	// Start the message listening goroutine
	go c.listenForMessages()

	log.Println("Bark Forwarder (WebSocket) plugin enabled.")
	return nil
}

// Disable disables the plugin.
func (c *BarkForwardPlugin) Disable() error {
	// Signal the goroutine to stop by closing the done channel
	if c.done != nil {
		close(c.done)
	}
	log.Println("Bark Forwarder (WebSocket) plugin disabled.")
	return nil
}

// GetDisplay implements plugin.Displayer.
// This method provides instructions on the plugin's page in the Gotify UI.
func (c *BarkForwardPlugin) GetDisplay(location *url.URL) string {
	return `
### Bark Forwarder 设置说明

本插件是为将 Gotify 消息转发到 Bark 而设计的专用插件。它通过连接到 Gotify 的 WebSocket 消息流来接收所有消息，并将其转发到你的 Bark 客户端。

**请按以下步骤配置：**

1.  **Gotify Host**: 填写你的 Gotify 服务器的 WebSocket 地址。
    -   例如: 如果你的 Gotify 访问地址是 'http://192.168.1.10:8080'，这里就填 'ws://192.168.1.10:8080'。
    -   如果使用了 HTTPS，请使用 'wss://'。

2.  **Gotify Client Token**: 需要为本插件创建一个专用的客户端 Token。
    -   请前往 Gotify 的 **"Clients"** 标签页。
    -   点击 "Create Client"，取一个名字（例如 'bark-plugin-client'）。
    -   **复制生成的 Token** 并粘贴到此处。

3.  **Bark Device Key**: 填写你的 Bark 客户端对应的设备 Key。

4.  **Bark URL**: 通常保持默认的 'https://api.day.app/push' 即可。

5.  **Reconnect Delay**: WebSocket 断线后自动重连的等待时间（秒），默认为 10 秒。

6.  **Encryption Key** (可选): AES 加密密钥。
    -   支持 16 字节 (AES-128) 或 32 字节 (AES-256)。
    -   留空则不启用加密。

7.  **Encryption IV** (可选): AES CBC 初始化向量，必须为 16 字节。

**重要提示**: 每次修改配置后，请**禁用**再**重新启用**本插件以使新配置生效。
`
}

// listenForMessages connects to the Gotify WebSocket and forwards messages.
// It runs in a loop to handle reconnections automatically.
func (c *BarkForwardPlugin) listenForMessages() {
	defer log.Println("Bark Forwarder: Listener stopped.")

	wsURL := c.config.GotifyHost + "/stream?token=" + c.config.GotifyClientToken

	for {
		// Check for disable signal before attempting to connect.
		select {
		case <-c.done:
			return
		default:
		}

		log.Printf("Bark Forwarder: Connecting to %s", wsURL)
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			log.Printf("Bark Forwarder: WebSocket dial error: %v", err)
			log.Printf("Bark Forwarder: Reconnecting in %d seconds...", c.config.ReconnectDelay)
			// Wait before reconnecting, but also listen for the done signal.
			select {
			case <-time.After(time.Duration(c.config.ReconnectDelay) * time.Second):
				continue
			case <-c.done:
				return
			}
		}

		log.Println("Bark Forwarder: Successfully connected to Gotify WebSocket.")

		// We have a connection, now we read messages until an error or disable signal.
		err = c.readMessages(conn)
		_ = conn.Close() // Ensure connection is closed on error or clean exit.

		if err == nil {
			// If readMessages returned nil, it means a clean shutdown was requested.
			return
		}

		log.Printf("Bark Forwarder: Disconnected: %v", err)

		// If we are here, it's due to a read error. Wait before reconnecting.
		select {
		case <-time.After(time.Duration(c.config.ReconnectDelay) * time.Second):
			continue
		case <-c.done:
			return
		}
	}
}

// readMessages is a helper function to read messages from an active WebSocket connection.
// It returns an error if the connection is broken, or nil if it's cleanly closed via the 'done' channel.
func (c *BarkForwardPlugin) readMessages(conn *websocket.Conn) error {
	// Use a separate goroutine to monitor done channel and close connection
	// This avoids the "repeated read on failed websocket connection" panic
	// that occurs when using ReadDeadline timeouts
	closedByDone := make(chan struct{})
	var closeClosedByDone sync.Once
	safeClose := func() {
		closeClosedByDone.Do(func() {
			close(closedByDone)
		})
	}
	go func() {
		select {
		case <-c.done:
			log.Println("Bark Forwarder: Received disable signal, closing WebSocket.")
			_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			_ = conn.Close()
			safeClose()
		case <-closedByDone:
			// Connection closed by read error, exit goroutine
		}
	}()

	defer func() {
		// Signal the monitor goroutine to exit if still running
		safeClose()
	}()

	for {
		_, messageBytes, err := conn.ReadMessage()
		if err != nil {
			// Check if closed by done signal
			select {
			case <-c.done:
				return nil // Clean shutdown
			default:
			}
			return err // Connection error
		}

		var msg plugin.Message
		if err := json.Unmarshal(messageBytes, &msg); err != nil {
			log.Printf("Bark Forwarder: Error unmarshalling message: %v", err)
			continue
		}

		// Forward the message to Bark
		if err := c.forwardToBark(msg); err != nil {
			log.Printf("Bark Forwarder: Failed to forward message to Bark: %v", err)
		}
	}
}

// forwardToBark sends the message to the configured Bark server.
func (c *BarkForwardPlugin) forwardToBark(msg plugin.Message) error {
	log.Printf("Bark Forwarder: Forwarding message '%s' to Bark.", msg.Title)

	barkReq := map[string]any{
		"device_key": c.config.BarkDeviceKey,
		"title":      msg.Title,
		"body":       msg.Message,
	}

	// Extract optional params from extras
	if extras, ok := msg.Extras["bark::params"]; ok {
		if params, ok := extras.(map[string]any); ok {
			maps.Copy(barkReq, params)
		}
	}

	var requestBody []byte
	var err error

	// Check if encryption is enabled
	var targetURL string
	if c.config.EncryptionKey != "" && c.config.EncryptionIV != "" {
		// Encrypt the request
		jsonValue, err := json.Marshal(barkReq)
		if err != nil {
			return fmt.Errorf("could not marshal bark request: %w", err)
		}

		ciphertext, err := c.encryptAESCBC(jsonValue)
		if err != nil {
			return fmt.Errorf("encryption failed: %w", err)
		}

		encryptedReq := map[string]string{
			"ciphertext": ciphertext,
		}
		requestBody, err = json.Marshal(encryptedReq)
		if err != nil {
			return fmt.Errorf("could not marshal encrypted request: %w", err)
		}
		log.Println("Bark Forwarder: Using encrypted push.")
		targetURL = encryptedPushURL(c.config.BarkURL, c.config.BarkDeviceKey)
	} else {
		// Plain request
		requestBody, err = json.Marshal(barkReq)
		if err != nil {
			return fmt.Errorf("could not marshal bark request: %w", err)
		}
		targetURL = plainPushURL(c.config.BarkURL)
	}

	resp, err := c.httpClient.Post(targetURL, "application/json", bytes.NewBuffer(requestBody))
	if err != nil {
		return fmt.Errorf("http post to bark failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bark server returned non-200 status: %s", resp.Status)
	}

	log.Println("Bark Forwarder: Successfully forwarded message to Bark.")
	return nil
}

// encryptAESCBC encrypts plaintext using AES-CBC with PKCS7 padding.
func (c *BarkForwardPlugin) encryptAESCBC(plaintext []byte) (string, error) {
	key := []byte(c.config.EncryptionKey)
	iv := []byte(c.config.EncryptionIV)

	// Validate key length (16 for AES-128, 32 for AES-256)
	if len(key) != 16 && len(key) != 32 {
		return "", errors.New("encryption key must be 16 or 32 bytes")
	}

	// Validate IV length (must be 16 bytes for AES)
	if len(iv) != aes.BlockSize {
		return "", fmt.Errorf("encryption IV must be %d bytes", aes.BlockSize)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("failed to create cipher: %w", err)
	}

	// Apply PKCS7 padding
	padding := aes.BlockSize - len(plaintext)%aes.BlockSize
	padText := bytes.Repeat([]byte{byte(padding)}, padding)
	plaintext = append(plaintext, padText...)

	// Encrypt
	ciphertext := make([]byte, len(plaintext))
	mode := cipher.NewCBCEncrypter(block, iv)
	mode.CryptBlocks(ciphertext, plaintext)

	// Return base64 encoded ciphertext
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// NewGotifyPluginInstance creates a plugin instance.
func NewGotifyPluginInstance(ctx plugin.UserContext) plugin.Plugin {
	return &BarkForwardPlugin{}
}

func main() {
	// This plugin is not meant to be run as a standalone application.
	// It should be built as a Go plugin and loaded by Gotify.
	panic("this should be built as go plugin")
}

func plainPushURL(baseURL string) string {
	if baseURL == "" {
		return "https://api.day.app/push"
	}
	baseURL = strings.TrimSpace(baseURL)
	baseURL = strings.TrimRight(baseURL, "/")
	if strings.HasSuffix(baseURL, "/push") {
		return baseURL
	}
	return baseURL + "/push"
}

func encryptedPushURL(baseURL, deviceKey string) string {
	if baseURL == "" {
		baseURL = "https://api.day.app"
	}
	baseURL = strings.TrimSpace(baseURL)
	baseURL = strings.TrimSuffix(baseURL, "/push")
	baseURL = strings.TrimRight(baseURL, "/")
	return fmt.Sprintf("%s/%s", baseURL, deviceKey)
}
