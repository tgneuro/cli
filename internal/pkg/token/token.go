package token

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/logrusorgru/aurora"
	"github.com/loophole/cli/internal/pkg/cache"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var logger *zap.Logger

const (
	deviceCodeURL = "https://owlsome.eu.auth0.com/oauth/device/code"
	tokenURL      = "https://owlsome.eu.auth0.com/oauth/token"
	clientID      = "R569dcCOUErjw1xVZOzqc7OUCiGTYNqN"
	scope         = "openid offline_access"
	audience      = "https://api.loophole.cloud"
)

func init() {
	atomicLevel := zap.NewAtomicLevel()
	encoderCfg := zap.NewProductionEncoderConfig()
	logger = zap.New(zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderCfg),
		zapcore.Lock(os.Stdout),
		atomicLevel,
	))

	atomicLevel.SetLevel(zap.DebugLevel)
}

type DeviceCodeSpec struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
	VeritificationURI       string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
}

type AuthError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

type TokenSpec struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
}

func IsTokenSaved() bool {
	tokensLocation := cache.GetLocalStorageFile("tokens.json")

	if _, err := os.Stat(tokensLocation); os.IsNotExist(err) {
		return false
	} else if err != nil {
		logger.Fatal("There was a problem reading tokens file", zap.Error(err))
	}
	return true
}

func SaveToken(token *TokenSpec) error {
	tokensLocation := cache.GetLocalStorageFile("tokens.json")

	tokenBytes, err := json.Marshal(token)
	if err != nil {
		return fmt.Errorf("There was a problem encoding tokens: %v", err)
	}
	err = ioutil.WriteFile(tokensLocation, tokenBytes, 0644)
	if err != nil {
		return fmt.Errorf("There was a problem writing tokens file: %v", err)
	}
	return nil
}

func RegisterDevice() (*DeviceCodeSpec, error) {
	payload := strings.NewReader(fmt.Sprintf("client_id=%s&scope=%s&audience=%s", url.QueryEscape(clientID), url.QueryEscape(scope), url.QueryEscape(audience)))

	req, err := http.NewRequest("POST", deviceCodeURL, payload)
	if err != nil {
		return nil, fmt.Errorf("There was a problem creating HTTP POST request for device code")
	}
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("There was a problem executing request for device code")
	}

	defer res.Body.Close()
	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("There was a problem reading device token response body")
	}

	var jsonResponseBody DeviceCodeSpec
	err = json.Unmarshal(body, &jsonResponseBody)
	if err != nil {
		return nil, fmt.Errorf("There was a problem decoding device token response body")
	}

	fmt.Printf("Please open %s and use %s code to log in\n", aurora.Yellow(jsonResponseBody.VeritificationURI), aurora.Yellow(jsonResponseBody.UserCode))

	return &jsonResponseBody, nil
}

func PollForToken(deviceCode string, interval int) (*TokenSpec, error) {
	grantType := "urn:ietf:params:oauth:grant-type:device_code"

	pollingInterval := time.Duration(interval) * time.Second
	logger.Debug("Polling with interval", zap.Duration("interval", pollingInterval), zap.String("unit", "second"))

	for {
		payload := strings.NewReader(fmt.Sprintf("grant_type=%s&device_code=%s&client_id=%s", url.QueryEscape(grantType), url.QueryEscape(deviceCode), url.QueryEscape(clientID)))

		req, err := http.NewRequest("POST", tokenURL, payload)
		if err != nil {
			logger.Debug("There was a problem creating HTTP POST request for token", zap.Error(err))
		}
		req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

		time.Sleep(pollingInterval)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			logger.Debug("There was a problem executing request for token", zap.Error(err))
			continue
		}
		defer res.Body.Close()
		body, err := ioutil.ReadAll(res.Body)
		if err != nil {
			logger.Debug("There was a problem reading token response body", zap.Error(err), zap.ByteString("body", body))
			continue
		}

		if res.StatusCode > 400 && res.StatusCode < 500 {
			var jsonResponseBody AuthError
			err := json.Unmarshal(body, &jsonResponseBody)
			if err != nil {
				logger.Debug("There was a problem decoding token response body", zap.Error(err), zap.ByteString("body", body))
				continue
			}
			logger.Debug("Error response", zap.String("error", jsonResponseBody.Error), zap.String("errorDescription", jsonResponseBody.ErrorDescription))
			if jsonResponseBody.Error == "authorization_pending" || jsonResponseBody.Error == "slow_down" {
				continue
			} else if jsonResponseBody.Error == "expired_token" || jsonResponseBody.Error == "invalid_grand" {
				return nil, fmt.Errorf("The device token expired, please reinitialize the login")
			} else if jsonResponseBody.Error == "access_denied" {
				return nil, fmt.Errorf("The device token got denied, please reinitialize the login")
			}
		} else if res.StatusCode >= 200 && res.StatusCode <= 300 {
			var jsonResponseBody TokenSpec
			err := json.Unmarshal(body, &jsonResponseBody)
			if err != nil {
				logger.Debug("There was a problem decoding token response body", zap.Error(err))
				continue
			}
			return &jsonResponseBody, nil
		} else {
			return nil, fmt.Errorf("Unexpected response from authorization server: %s", body)
		}
	}
}

func RefreshToken() error {
	grantType := "refresh_token"
	token, err := GetRefreshToken()
	if err != nil {
		return err
	}

	payload := strings.NewReader(fmt.Sprintf("grant_type=%s&client_id=%s&refresh_token=%s", url.QueryEscape(grantType), url.QueryEscape(clientID), url.QueryEscape(token)))

	req, _ := http.NewRequest("POST", tokenURL, payload)

	req.Header.Add("content-type", "application/x-www-form-urlencoded")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return err
	}

	if res.StatusCode > 400 && res.StatusCode < 500 {
		var jsonResponseBody AuthError
		err := json.Unmarshal(body, &jsonResponseBody)
		if err != nil {
			return err
		}
		logger.Debug("Error response", zap.String("error", jsonResponseBody.Error), zap.String("errorDescription", jsonResponseBody.ErrorDescription))
		if jsonResponseBody.Error == "expired_token" || jsonResponseBody.Error == "invalid_grand" {
			return fmt.Errorf("The device token expired, please reinitialize the login")
		} else if jsonResponseBody.Error == "access_denied" {
			return fmt.Errorf("The device token got denied, please reinitialize the login")
		}
	} else if res.StatusCode >= 200 && res.StatusCode <= 300 {
		var jsonResponseBody TokenSpec
		err := json.Unmarshal(body, &jsonResponseBody)
		if err != nil {
			return err
		}

		jsonResponseBody.RefreshToken = token

		err = SaveToken(&jsonResponseBody)
		if err != nil {
			return err
		}

	} else {
		return fmt.Errorf("Unexpected response from authorization server: %s", body)
	}
	return nil

}

func DeleteTokens() {
	tokensLocation := cache.GetLocalStorageFile("tokens.json")

	err := os.Remove(tokensLocation)
	if err != nil {
		logger.Fatal("There was a problem removing tokens file", zap.Error(err))
	}
}

func GetAccessToken() (string, error) {
	tokensLocation := cache.GetLocalStorageFile("tokens.json")

	tokens, err := ioutil.ReadFile(tokensLocation)
	if err != nil {
		return "", fmt.Errorf("There was a problem reading tokens: %v", err)
	}
	var token TokenSpec
	err = json.Unmarshal(tokens, &token)
	if err != nil {
		return "", fmt.Errorf("There was a problem decoding tokens: %v", err)
	}
	return token.AccessToken, nil
}

func GetRefreshToken() (string, error) {
	tokensLocation := cache.GetLocalStorageFile("tokens.json")

	tokens, err := ioutil.ReadFile(tokensLocation)
	if err != nil {
		return "", fmt.Errorf("There was a problem reading tokens: %v", err)
	}
	var token TokenSpec
	err = json.Unmarshal(tokens, &token)
	if err != nil {
		return "", fmt.Errorf("There was a problem decoding tokens: %v", err)
	}
	return token.RefreshToken, nil
}
