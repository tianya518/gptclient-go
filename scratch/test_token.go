package main

import (
	"fmt"
	"regexp"
)

var accessTokenRegex = regexp.MustCompile(`"accessToken"\s*:\s*"([^"]+)"`)

func cleanToken(t string) string {
	if match := accessTokenRegex.FindStringSubmatch(t); len(match) == 2 {
		return match[1]
	}
	return t
}

func main() {
	sample := `{"WARNING_BANNER":"!!!!!!!!!!!!!!!!!!!! DO NOT SHARE ANY PART OF THE INFORMATION YOU SEE HERE. THIS INFORMATION IS SENSITIVE AND CAN GRANT ACCESS TO YOUR ACCOUNT. SHARING THIS INFORMATION IS LIKE SHARING YOUR PASSWORD. !!!!!!!!!!!!!!!!!!!!","user":{"id":"user-zazHhlc0lHmSlxiTVVnP4Y4r","name":"渣渣","email":"5661@163.com","idp":"auth0","iat":1776085551,"amr":["otp","urn:openai:amr:otp_email"],"mfa":false},"expires":"2026-07-28T09:45:53.221Z","account":{"id":"8148c243-b83b-49a2-be81-12adb24448f6","planType":"plus","structure":"personal","isConversationClassifierEnabledForWorkspace":true,"isFinservEnabledWorkspace":false,"isFedrampCompliantWorkspace":false,"isDelinquent":false,"residencyRegion":"no_constraint","computeResidency":"no_constraint"},"accessToken":"eyJhbGciOiJSUzI1NiIsImtpZ...","authProvider":"openai"}`
	fmt.Println("Result:", cleanToken(sample))
}
