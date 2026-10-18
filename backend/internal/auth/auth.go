// Package auth — bcrypt-хэширование паролей и JWT-токены.
package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// Claims — полезная нагрузка JWT.
type Claims struct {
	UserID string `json:"sub_uid"`
	Email  string `json:"email"`
	jwt.RegisteredClaims
}

// HashPassword возвращает bcrypt-хэш пароля (стоимость по умолчанию).
func HashPassword(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("auth: hash password: %w", err)
	}
	return string(b), nil
}

// CheckPassword сравнивает пароль с хэшем.
func CheckPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// GenerateToken создаёт подписанный HS256 JWT для пользователя.
func GenerateToken(secret, userID, email string, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := Claims{
		UserID: userID,
		Email:  email,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			Issuer:    "rathole-control-plane",
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		return "", fmt.Errorf("auth: sign token: %w", err)
	}
	return signed, nil
}

// ParseToken валидирует токен и возвращает claims. Ошибки токена — единый ErrInvalidToken,
// чтобы не раскрывать причину (expired/invalid signature) нарушителю.
var ErrInvalidToken = errors.New("auth: invalid token")

func ParseToken(secret, tokenString string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(t *jwt.Token) (interface{}, error) {
		// Жёсткая проверка алгоритма — защита от атаки «alg: none» / подмены алгоритма.
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("auth: unexpected signing method %v", t.Header["alg"])
		}
		return []byte(secret), nil
	})
	if err != nil || !token.Valid {
		return nil, ErrInvalidToken
	}
	if claims.UserID == "" {
		return nil, ErrInvalidToken
	}
	return claims, nil
}
