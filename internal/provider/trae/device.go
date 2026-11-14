// Package trae 适配字节跳动的 Trae CN（Cloud IDE）。
//
// 这家不是标准 OpenAI 兼容，需要三样东西才能用：
//  1. 设备身份：EC P-256 密钥对 + 16 位数字 deviceId + 32 位 hex machineId
//  2. 设备证明：对 "POST\n{path}\n{clientId}\n{refresh}\n{at}\n{nonce}" 做 ECDSA 签名
//  3. 客户端伪装：请求头自称 Windows + AMD + 特定 IDE 版本
//
// 认证流程：浏览器打开 trae.cn/authorization → 回调 127.0.0.1 拿 userJwt
// → 用 refresh 换 Cloud-IDE-JWT token（ExchangeToken）。
package trae

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
)

const (
	// ExchangePath 是换取 token 的接口。
	ExchangePath = "/trae/api/v3/oauth/ExchangeToken"
	// Brand 伪装成这台机器（官方客户端上报的型号）。
	Brand = "ASUS TUF Gaming A15 FA507RM_FA507RM"
	// AppID 是 Trae 桌面端的固定 app id。
	AppID = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	// AuthHost 是认证域。
	AuthHost = "https://api.trae.cn"
)

// ClientCandidates 是两个可选的客户端身份，第一个被拒时换第二个再试。
var ClientCandidates = []struct {
	ClientID string
	Platform string
	Version  string
}{
	{"en1oxy7wnw8j9n", "SOLO_PC", "0.1.64"},
	{"ono9krqynydwx5", "IDE_PC", "3.3.99"},
}

// Identity 是一个设备的身份（对应 All-in 里的 TraeDevice）。
type Identity struct {
	DeviceID   string `json:"deviceId"`   // 16 位数字，首位非 0
	MachineID  string `json:"machineId"`  // 32 位 hex
	PublicKey  string `json:"publicKey"`  // PKIX PEM
	PrivateKey string `json:"privateKey"` // PKCS8 PEM

	priv *ecdsa.PrivateKey
}

// NewIdentity 生成一套全新的设备身份。
func NewIdentity() (*Identity, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("生成 EC 密钥失败: %w", err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	pkix, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, err
	}

	// deviceId: 16 位数字，首位 1-9
	digits := make([]byte, 16)
	first, err := rand.Int(rand.Reader, big.NewInt(9))
	if err != nil {
		return nil, err
	}
	digits[0] = byte('1' + first.Int64())
	for i := 1; i < 16; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return nil, err
		}
		digits[i] = byte('0' + n.Int64())
	}

	machine := make([]byte, 16)
	if _, err := rand.Read(machine); err != nil {
		return nil, err
	}

	return &Identity{
		DeviceID:   string(digits),
		MachineID:  hex.EncodeToString(machine),
		PublicKey:  string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pkix})),
		PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})),
		priv:       key,
	}, nil
}

// LoadIdentity 从已保存的 PEM 恢复身份。
func LoadIdentity(deviceID, machineID, publicPEM, privatePEM string) (*Identity, error) {
	block, _ := pem.Decode([]byte(privatePEM))
	if block == nil {
		return nil, fmt.Errorf("私钥 PEM 解析失败")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("私钥解析失败: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("不是 ECDSA 私钥")
	}
	if key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("不是 P-256 曲线")
	}
	if len(deviceID) != 16 || deviceID[0] == '0' {
		return nil, fmt.Errorf("deviceId 非法")
	}
	if len(machineID) != 32 {
		return nil, fmt.Errorf("machineId 非法")
	}
	return &Identity{
		DeviceID: deviceID, MachineID: machineID,
		PublicKey: publicPEM, PrivateKey: privatePEM, priv: key,
	}, nil
}

// Proof 生成设备证明：对 "POST\n{path}\n{clientId}\n{refresh}\n{ts}\n{nonce}" 签名。
func (id *Identity) Proof(path, clientID, refresh string, at int64, nonce string) (string, error) {
	if id.priv == nil {
		return "", fmt.Errorf("设备私钥未加载")
	}
	msg := fmt.Sprintf("POST\n%s\n%s\n%s\n%d\n%s", path, clientID, refresh, at, nonce)
	sum := sha256.Sum256([]byte(msg))
	r, s, err := ecdsa.Sign(rand.Reader, id.priv, sum[:])
	if err != nil {
		return "", err
	}
	// 转成 IEEE P1363（r||s 各 32 字节），与 Java 的 SHA256withECDSA 输出格式对齐
	out := make([]byte, 64)
	r.FillBytes(out[:32])
	s.FillBytes(out[32:])
	return base64.StdEncoding.EncodeToString(out), nil
}

// RandomNonce 生成 16 字节 hex 随机串。
func RandomNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
