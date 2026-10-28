package llmprovider

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"

	"github.com/wuzuxian/interview/internal/apperr"
)

// DevFallbackKey 与 Java 版 DEV_FALLBACK_KEY 一致，仅显式允许时启用。
const DevFallbackKey = "interview-guide-dev-only-provider-api-key-encryption"

// CryptoService Provider API Key 的 AES-256-GCM 加解密，密文 Base64 存储。
// 密钥来源与 Java 版一致：优先配置的 api-key-encryption-key，
// 支持 32 字节 Base64 原文或任意字符串 SHA-256 派生。
type CryptoService struct {
	aead cipher.AEAD
}

// NewCryptoService 根据配置密钥创建加解密服务。
// encryptionKey 为空且 allowFallback=false 时返回错误（与 Java 版校验一致）。
func NewCryptoService(encryptionKey string, allowFallback bool) (*CryptoService, error) {
	key := encryptionKey
	if key == "" {
		if !allowFallback {
			return nil, apperr.New(apperr.CodeProviderReadFailed,
				"APP_AI_CONFIG_ENCRYPTION_KEY 未配置，无法初始化 Provider API Key 加密")
		}
		key = DevFallbackKey
	}
	keyBytes, err := resolveKeyBytes(key)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(keyBytes)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeProviderReadFailed, "初始化 Provider API Key 加密密钥失败", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeProviderReadFailed, "初始化 Provider API Key GCM 失败", err)
	}
	return &CryptoService{aead: aead}, nil
}

// resolveKeyBytes Base64 32 字节优先，否则 SHA-256 派生，与 Java resolveKeyBytes 一致。
func resolveKeyBytes(configured string) ([]byte, error) {
	if decoded, err := base64.StdEncoding.DecodeString(configured); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	sum := sha256.Sum256([]byte(configured))
	return sum[:], nil
}

// EncryptedValue nonce 与 ciphertext 均为 Base64 字符串。
type EncryptedValue struct {
	Nonce      string
	Ciphertext string
}

// Encrypt 加密明文；nonce 12 字节，GCM tag 128 位，与 Java 版参数一致。
func (s *CryptoService) Encrypt(plainText string) (EncryptedValue, error) {
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return EncryptedValue{}, apperr.Wrap(apperr.CodeProviderWriteFailed, "加密 Provider API Key 失败", err)
	}
	ct := s.aead.Seal(nil, nonce, []byte(plainText), nil)
	return EncryptedValue{
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(ct),
	}, nil
}

// Decrypt 解密；失败时返回与 Java 版一致的错误语义。
func (s *CryptoService) Decrypt(nonceBase64, ciphertextBase64 string) (string, error) {
	nonce, err := base64.StdEncoding.DecodeString(nonceBase64)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeProviderReadFailed, "解密 Provider API Key 失败，请检查 APP_AI_CONFIG_ENCRYPTION_KEY", err)
	}
	ct, err := base64.StdEncoding.DecodeString(ciphertextBase64)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeProviderReadFailed, "解密 Provider API Key 失败，请检查 APP_AI_CONFIG_ENCRYPTION_KEY", err)
	}
	plain, err := s.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeProviderReadFailed,
			fmt.Sprintf("解密 Provider API Key 失败，请检查 APP_AI_CONFIG_ENCRYPTION_KEY: %v", err), err)
	}
	return string(plain), nil
}

// MaskAPIKey 打码展示：前 3 位 + "***" + 后 3 位；长度不足 7 返回 "***"。
func MaskAPIKey(apiKey string) string {
	if len(apiKey) <= 6 {
		return "***"
	}
	return apiKey[:3] + "***" + apiKey[len(apiKey)-3:]
}
