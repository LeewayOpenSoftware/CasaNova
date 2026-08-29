package httper

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/LeewayOpenSoftware/CasaNova/pkg/config"
	"github.com/tidwall/gjson"
)

// GET
// url:
// response:
func Get(url string, head map[string]string) (response string) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return ""
	}

	for k, v := range head {
		req.Header.Add(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		fmt.Println(err)
		return ""
	}
	defer resp.Body.Close()
	var buffer [512]byte
	result := bytes.NewBuffer(nil)
	for {
		n, err := resp.Body.Read(buffer[0:])
		result.Write(buffer[0:n])
		if err != nil && err == io.EOF {
			break
		} else if err != nil {
			// logger.Error(err)
			return ""
			//	panic(err)
		}
	}
	response = result.String()
	return
}

// GET
// url:
// response:
func PersonGet(url string) (response string) {
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return ""
	}
	resp, err := client.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		return ""
	}
	defer resp.Body.Close()
	var buffer [512]byte
	result := bytes.NewBuffer(nil)
	for {
		n, err := resp.Body.Read(buffer[0:])
		result.Write(buffer[0:n])
		if err != nil && err == io.EOF {
			break
		} else if err != nil {
			// logger.Error(err)
			return ""
			//	panic(err)
		}
	}
	response = result.String()
	return
}

// POST
// url:data:POST,contentType:application/json
// content:
func Post(url string, data []byte, contentType string, head map[string]string) (content string) {
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(data))
	if err != nil {
		fmt.Println("httper.Post: failed to create request:", err)
		return ""
	}
	req.Header.Add("content-type", contentType)
	for k, v := range head {
		req.Header.Add(k, v)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		fmt.Println("httper.Post: request failed:", err)
		return ""
	}
	defer resp.Body.Close()

	result, _ := io.ReadAll(resp.Body)
	content = string(result)
	return
}

// ZeroTierGet performs a GET request with custom headers.
func ZeroTierGet(url string, head map[string]string) (content string, code int) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		fmt.Println("httper.ZeroTierGet: failed to create request:", err)
		return "", 0
	}
	for k, v := range head {
		req.Header.Add(k, v)
	}

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
		fmt.Println("httper.ZeroTierGet: request failed:", err)
		return "", 0
	}
	defer resp.Body.Close()
	code = resp.StatusCode
	result, _ := io.ReadAll(resp.Body)
	content = string(result)
	return
}

// GET
// url:
// response:
func OasisGet(url string) (response string) {
	head := make(map[string]string)

	t := make(chan string, 1)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				t <- ""
			}
		}()
		// Skip the remote /token round-trip when ServerApi is unset
		// (CasaNova's default — no built-in phone-home). Empty token
		// lets the caller proceed with whatever auth headers it
		// already supplied; calls that actually require an OasisGet
		// token will fail downstream with a clearer error than a
		// bogus "https:///token" connection refusal.
		if config.ServerInfo.ServerApi == "" {
			t <- ""
			return
		}
		str := Get(config.ServerInfo.ServerApi+"/token", nil)

		t <- gjson.Get(str, "data").String()
	}()
	head["Authorization"] = <-t

	return Get(url, head)
}
