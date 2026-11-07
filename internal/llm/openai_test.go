package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func TestOpenAIOptionalOutputBudget(t *testing.T) {
	for _, limit := range []int{0, 3000} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				value, exists := body["max_tokens"]
				if limit == 0 && exists {
					t.Error("changed default generation budget")
				}
				if limit > 0 && (!exists || value != float64(limit)) {
					t.Errorf("missing output budget: %v", body)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": "result"}}}})
			}))
			defer srv.Close()
			client, err := New(Config{Kind: "api", BaseURL: srv.URL, ModelID: "test", MaxOutputTokens: limit})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = client.Generate(context.Background(), "bounded request", nil); err != nil {
				t.Fatal(err)
			}
		})
	}
}
