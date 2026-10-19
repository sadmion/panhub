package cloud139

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// calSign 计算 mcloud-sign 的第三段。
//
// 算法（逆向自公开实现，与 AList/OpenList 的 calSign 一致）：
//  1. 去掉 body 里的换行与空格
//  2. URL 编码（+ 转成 %20）
//  3. 拆成单个字符后按字典序排序
//  4. Base64
//  5. md5(base64) + md5("时间戳:随机串")
//  6. 把上一步结果再 md5 一次并转大写
//
// 注意：这套算法对 body 的原始字节极其敏感，所以请求体必须由我们自己
// 序列化后原样发送，不能交给 http 层再 marshal 一次。
func calSign(body, ts, randStr string) string {
	body = strings.ReplaceAll(body, "\n", "")
	body = strings.ReplaceAll(body, " ", "")
	body = strings.ReplaceAll(url.QueryEscape(body), "+", "%20")

	chars := strings.Split(body, "")
	sort.Strings(chars)

	b64 := base64.StdEncoding.EncodeToString([]byte(strings.Join(chars, "")))
	seed := md5Hex(b64) + md5Hex(ts+":"+randStr)
	return strings.ToUpper(md5Hex(seed))
}

// sortedJSON 序列化为「键按字典序、紧凑、不转义 HTML」的 JSON。
// 139 的登录接口会校验这个顺序，且 clientkey_decrypt 里含 & 等字符，
// 一旦被转义成 \u0026 就会失败。
func sortedJSON(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

func pkcs7Pad(data []byte, blockSize int) []byte {
	pad := blockSize - len(data)%blockSize
	return append(data, bytes.Repeat([]byte{byte(pad)}, pad)...)
}

func pkcs7Unpad(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("pkcs7: 数据为空")
	}
	n := int(data[len(data)-1])
	if n == 0 || n > len(data) || n > aes.BlockSize {
		return nil, errors.New("pkcs7: 填充非法")
	}
	return data[:len(data)-n], nil
}

func aesCBCEncrypt(plain, key, iv []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(iv) != block.BlockSize() {
		return nil, fmt.Errorf("AES-CBC: IV 长度应为 %d", block.BlockSize())
	}
	padded := pkcs7Pad(plain, block.BlockSize())
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
	return out, nil
}

func aesCBCDecrypt(ct, key, iv []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(ct)%block.BlockSize() != 0 {
		return nil, errors.New("AES-CBC: 密文长度不是块大小的整数倍")
	}
	out := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, ct)
	return pkcs7Unpad(out)
}

func aesECBDecrypt(ct, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(ct)%block.BlockSize() != 0 {
		return nil, errors.New("AES-ECB: 密文长度不是块大小的整数倍")
	}
	out := make([]byte, len(ct))
	size := block.BlockSize()
	for i := 0; i < len(ct); i += size {
		block.Decrypt(out[i:i+size], ct[i:i+size])
	}
	return pkcs7Unpad(out)
}

func randomIV() ([]byte, error) {
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(iv); err != nil {
		return nil, err
	}
	return iv, nil
}
