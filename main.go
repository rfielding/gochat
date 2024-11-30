package main

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// Configuration structures
type Configuration struct {
	XMLName   xml.Name `xml:"configuration"`
	SiteTitle string   `xml:"site_title"`
	BaseURL   string   `xml:"base_url"`
	Templates struct {
		Template []struct {
			Name string `xml:"name,attr"`
			HTML string `xml:",chardata"`
		} `xml:"template"`
	} `xml:"templates"`
	Forms struct {
		Form []struct {
			Name   string `xml:"name,attr"`
			Config string `xml:"config"`
			Fields string `xml:"form_fields"`
			Prompt string `xml:"system_prompt"`
		} `xml:"form"`
	} `xml:"forms"`
}

// Chat structures
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatResponse struct {
	Choices []struct {
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

type ChatSession struct {
	Messages []ChatMessage
	FormData map[string]string
}

// Global session storage
var chatSessions = make(map[string]*ChatSession)

func main() {
	// Load configuration
	data, err := ioutil.ReadFile("configuration.xml")
	if err != nil {
		log.Fatalf("Error reading config: %v", err)
	}

	var config Configuration
	if err := xml.Unmarshal(data, &config); err != nil {
		log.Fatalf("Error parsing config: %v", err)
	}

	// Home page handler
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}

		// Find home template
		var homeHTML string
		for _, tmpl := range config.Templates.Template {
			if tmpl.Name == "home_page" {
				homeHTML = tmpl.HTML
				break
			}
		}

		tmpl := template.Must(template.New("home").Parse(homeHTML))
		tmpl.Execute(w, config)
	})

	// QR code handler
	http.HandleFunc("/qr/", func(w http.ResponseWriter, r *http.Request) {
		formPath := strings.TrimPrefix(r.URL.Path, "/qr/")
		formURL := config.BaseURL + "/form/" + formPath

		png, err := qrcode.Encode(formURL, qrcode.Medium, 256)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "image/png")
		w.Write(png)
	})

	// Set up form handlers
	for _, form := range config.Forms.Form {
		formName := form.Name
		formPath := "/form/" + formName

		log.Printf("Setting up handlers for form: %s at path: %s", formName, formPath)

		// Form page handler
		http.HandleFunc(formPath, func(w http.ResponseWriter, r *http.Request) {
			var formHTML string
			for _, tmpl := range config.Templates.Template {
				if tmpl.Name == "chat_form" {
					formHTML = tmpl.HTML
					break
				}
			}

			tmpl := template.Must(template.New("form").Parse(formHTML))
			tmpl.Execute(w, form)
		})

		// Chat endpoint
		http.HandleFunc(formPath+"/chat", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
				return
			}
			handleChat(w, r, form)
		})
	}

	log.Printf("Server starting on %s", config.BaseURL)
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func handleChat(w http.ResponseWriter, r *http.Request, form struct {
	Name   string `xml:"name,attr"`
	Config string `xml:"config"`
	Fields string `xml:"form_fields"`
	Prompt string `xml:"system_prompt"`
}) {
	var chatReq struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&chatReq); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	// Get or create session
	session := chatSessions[form.Name]
	if session == nil {
		session = &ChatSession{
			Messages: []ChatMessage{
				{
					Role:    "system",
					Content: fmt.Sprintf(form.Prompt, form.Fields),
				},
			},
			FormData: make(map[string]string),
		}
		chatSessions[form.Name] = session
	}

	// Add user message
	session.Messages = append(session.Messages, ChatMessage{
		Role:    "user",
		Content: chatReq.Message,
	})

	// Call OpenAI
	reqBody, _ := json.Marshal(map[string]interface{}{
		"model":    "gpt-4",
		"messages": session.Messages,
	})

	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions",
		bytes.NewBuffer(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+os.Getenv("OPENAI_API_KEY"))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "AI service error", http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	var aiResp ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&aiResp); err != nil {
		http.Error(w, "Invalid AI response", http.StatusInternalServerError)
		return
	}

	if len(aiResp.Choices) > 0 {
		aiMessage := aiResp.Choices[0].Message
		session.Messages = append(session.Messages, ChatMessage{
			Role:    aiMessage.Role,
			Content: aiMessage.Content,
		})

		json.NewEncoder(w).Encode(map[string]string{
			"response": aiMessage.Content,
		})
	}
}
