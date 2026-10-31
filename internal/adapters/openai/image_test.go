package openai

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sarpaydemir/dost/internal/testkit"
	"github.com/sarpaydemir/dost/internal/testkit/golden"
	"github.com/sarpaydemir/dost/internal/testkit/openaifake"
)

const (
	png = "iVBORw0KGgpQSVhFTFM=" // "\x89PNG\r\n\x1a\nPIXELS"
	jpg = "/9j/SlBFRw=="         // "\xff\xd8\xffJPEG"
)

func TestImageBody(t *testing.T) {
	fake := openaifake.New(t)
	fake.Chat().Reply(openaifake.Completion(openaifake.Images("data:image/png;base64," + png)))
	p, _ := open(t, fake.URL())
	if _, err := p.Image(context.Background(), "img-1", "Şu <b>kedi</b> & \"köpek\"\tC:\\yol\x01 satır ayırıcı 🙂"); err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "image", append(fake.Requests()[0].Body, '\n'))
}

func TestImageReadsTheFirstImageDataURL(t *testing.T) {
	message := func(m string) string {
		return `{"id":"x","choices":[{"index":0,"message":` + m + `,"finish_reason":"stop"}]}`
	}
	const pngData, jpgData = "\x89PNG\r\n\x1a\nPIXELS", "\xff\xd8\xffJPEG"
	for _, tc := range []struct {
		name, answer, mediaType, data, err string
	}{
		{"an image part in images", message(`{"images":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + png + `"}}]}`),
			"image/png", pngData, ""},
		{"an image's url", message(`{"images":[{"url":"data:image/jpeg;base64,` + jpg + `"}]}`), "image/jpeg", jpgData, ""},
		{"an image's url after a null image_url", message(`{"images":[{"image_url":{"url":null},"url":"data:image/png;base64,` + png + `"}]}`),
			"image/png", pngData, ""},
		{"an image's url after an empty image_url", message(`{"images":[{"image_url":{"url":""},"url":"data:image/png;base64,` + png + `"}]}`),
			"image/png", pngData, ""},
		{"a URL in images", message(`{"images":["data:image/png;base64,` + png + `"]}`), "image/png", pngData, ""},
		{"images before content", message(`{"content":[{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,` + jpg + `"}}],` +
			`"images":["data:image/png;base64,` + png + `"]}`), "image/png", pngData, ""},
		{"an image part in content", message(`{"content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,` + jpg + `"}}]}`),
			"image/jpeg", jpgData, ""},
		{"web URLs are skipped", message(`{"images":["https://cdn.example/x.png","data:image/jpeg;base64,` + jpg + `"]}`), "image/jpeg", jpgData, ""},
		{"data URLs without a comma are skipped", message(`{"images":["data:image/png;base64","data:image/jpeg;base64,` + jpg + `"]}`),
			"image/jpeg", jpgData, ""},
		{"the prefix is case sensitive", message(`{"images":[{"image_url":{"url":"DATA:image/png;base64,` + png + `"}},"data:image/jpeg;base64,` + jpg + `"]}`),
			"image/jpeg", jpgData, ""},
		{"URLs that are not text are skipped", message(`{"images":[{"url":5}],"content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + png + `"}}]}`),
			"image/png", pngData, ""},
		{"webp is labelled jpeg", message(`{"images":["data:image/webp;base64,` + png + `"]}`), "image/jpeg", pngData, ""},
		{"an uppercase PNG is labelled jpeg", message(`{"images":["data:image/PNG;base64,` + png + `"]}`), "image/jpeg", pngData, ""},
		{"png anywhere in the header", message(`{"images":["data:image/jpeg;name=x.png;base64,` + jpg + `"]}`), "image/png", jpgData, ""},
		{"data without a base64 marker", message(`{"images":["data:image/png,QUJD"]}`), "image/png", "ABC", ""},
		{"escaped characters", message(`{"images":["data:image\/png;base64,QU\/D"]}`), "image/png", "AO\xc3", ""},
		{"non-finite numbers elsewhere", `{"usage":{"cost":NaN},"choices":[{"message":{"images":["data:image/png;base64,QUJD"]}}]}`,
			"image/png", "ABC", ""},
		{"the first data URL decides", message(`{"images":["data:image/png;base64,QUJDR","data:image/png;base64,` + png + `"]}`),
			"", "", "Invalid base64-encoded string: number of data characters (5) cannot be 1 more than a multiple of 4"},
		{"text content", message(`{"content":"here you go"}`), "", "", "No image in response"},
		{"an image part without a url", message(`{"content":[{"type":"image_url"}]}`), "", "", "No image in response"},
		{"an empty message", message(`{}`), "", "", "No image in response"},
		{"a null message", message(`null`), "", "", "No image in response"},
		{"a choice without a message", `{"choices":[{}]}`, "", "", "No image in response"},
		{"no choices", `{"id":"x"}`, "", "", "No image in response"},
		{"empty choices", `{"choices":[]}`, "", "", "No image in response"},
		{"the last of repeated members", `{"choices":[{"message":{"images":["data:image/png;base64,QUJD"]}}],"choices":[]}`, "", "", "No image in response"},
		{"an error payload", `{"error":{"message":"bad","code":429}}`, "", "", "No image in response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := openaifake.New(t)
			fake.Chat().Reply(testkit.Raw(200, "application/json", []byte(tc.answer)))
			p, _ := open(t, fake.URL())
			img, err := p.Image(context.Background(), "img-1", "a cat")
			if tc.err != "" {
				if err == nil || err.Error() != tc.err {
					t.Errorf("got %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil || img.MediaType != tc.mediaType || string(img.Data) != tc.data {
				t.Errorf("got %s %q, %v; want %s %q", img.MediaType, img.Data, err, tc.mediaType, tc.data)
			}
		})
	}
}

func TestImageFailuresAreWordedForTheModelAndNotRetried(t *testing.T) {
	redirect := testkit.Raw(307, "text/plain", []byte("moved"))
	redirect.Header.Set("Location", "/elsewhere")
	for _, tc := range []struct {
		name   string
		answer testkit.Response
		err    string
	}{
		{"server error", testkit.Raw(500, "text/plain", []byte("upstream exploded")), "API error: 500 - upstream exploded"},
		{"rate limit without a body", testkit.Raw(429, "text/plain", nil), "API error: 429"},
		{"bad request", testkit.Raw(400, "application/json", []byte(`{"error":{"message":"bad"}}`)), `API error: 400 - {"error":{"message":"bad"}}`},
		{"long body", testkit.Raw(500, "text/plain", []byte(strings.Repeat("é", 300))), "API error: 500 - " + strings.Repeat("é", 250)},
		{"blank body", testkit.Raw(500, "text/plain", []byte("  ")), "API error: 500 -   "},
		{"redirect", redirect, "API error: 307 - moved"},
		{"html", testkit.Raw(200, "text/html; charset=utf-8", []byte(`{"choices":[]}`)), "Unexpected content-type: text/html; charset=utf-8"},
		{"uppercase content type", testkit.Raw(200, "TEXT/HTML", []byte(`{"choices":[]}`)), "Unexpected content-type: text/html"},
		{"no content type", testkit.Status(204), "Unexpected content-type: unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := openaifake.New(t)
			fake.Chat().Default(tc.answer)
			p, clk := open(t, fake.URL())
			if _, err := p.Image(context.Background(), "img-1", "a cat"); err == nil || err.Error() != tc.err {
				t.Errorf("got %v, want %q", err, tc.err)
			}
			if n, waits := len(fake.Requests()), clk.Waits(); n != 1 || len(waits) != 0 {
				t.Errorf("%d attempts and waits %v, want one attempt", n, waits)
			}
		})
	}
}

func TestImageAcceptsAnyJSONContentType(t *testing.T) {
	fake := openaifake.New(t)
	fake.Chat().Reply(testkit.Raw(200, "Application/JSON; charset=utf-8", []byte(`{"choices":[{"message":{"images":["data:image/png;base64,QUJD"]}}]}`)))
	p, _ := open(t, fake.URL())
	if img, err := p.Image(context.Background(), "img-1", "a cat"); err != nil || string(img.Data) != "ABC" {
		t.Errorf("got %q, %v; want ABC", img.Data, err)
	}
}

func TestImageTimeoutIsTwoMinutesWhateverTheRequestTimeout(t *testing.T) {
	fake := openaifake.New(t)
	fake.Chat().Reply(openaifake.Completion(openaifake.Images("data:image/png;base64,QUJD")).WithDelay(200 * time.Millisecond))
	fake.Chat().Default(paced(fake, step{time.Hour, ""}))
	p, clk := open(t, fake.URL(), `"request_timeout":0.05`)
	if got := p.(*provider).imageTimeout; got != 2*time.Minute {
		t.Errorf("image timeout %v, want 2m", got)
	}
	p.(*provider).imageTimeout = time.Second
	if img, err := p.Image(context.Background(), "img-1", "a cat"); err != nil || string(img.Data) != "ABC" {
		t.Errorf("answer slower than the request timeout: got %q, %v; want ABC", img.Data, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := p.Image(ctx, "img-1", "a cat"); err == nil || err.Error() != "Timeout" || ctx.Err() != nil {
		t.Errorf("answer that never comes: got %v (context: %v), want Timeout after the image timeout", err, ctx.Err())
	}
	if n, waits := len(fake.Requests()), clk.Waits(); n != 2 || len(waits) != 0 {
		t.Errorf("%d attempts and waits %v, want one attempt per image", n, waits)
	}
}

func TestImageRequestFailuresLeaveTheAPIAddressToTheLog(t *testing.T) {
	fake := openaifake.New(t)
	fake.Chat().Reply(testkit.ConnReset())
	fake.Chat().Default(paced(fake, step{time.Hour, ""}))
	var logs bytes.Buffer
	doc := fmt.Sprintf(`{"model":"m","api_base":%q,"key":%q}`, fake.URL(), key)
	p, err := New(settings(t, doc), http.DefaultTransport, &instantClock{}, "dost/1.2.3", slog.New(slog.NewTextHandler(&logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	address := fake.URL() + "/chat/completions"
	if _, err := p.Image(context.Background(), "img-1", "a cat"); err == nil || strings.Contains(err.Error(), address) {
		t.Errorf("reset connection: got %v, want an error without %s", err, address)
	}
	if !strings.Contains(logs.String(), address) {
		t.Errorf("logs %q, want the failed request with its address", logs.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := p.Image(ctx, "img-1", "a cat"); err == nil || err.Error() != "Timeout" {
		t.Errorf("deadline before the answer: got %v, want Timeout", err)
	}
}

func TestImageDataDecodesLikeALenientBase64Decoder(t *testing.T) {
	for _, tc := range []struct{ data, want, err string }{
		{"QU\nJD\t RE!@#$%^&*()_-~`.:;'\"<>?[]{}|\\RU", "ABCDDT", ""},
		{"-_QUJD", "ABC", ""},
		{"", "", ""},
		{"=QUJD", "ABC", ""},
		{"Q=UJD", "ABC", ""},
		{"QUI=", "AB", ""},
		{"QU==", "A", ""},
		{"QUI==", "AB", ""},
		{"QUJD====", "ABC", ""},
		{"QUI=QUJD", "AB", ""},
		{"QUI=x", "AB", ""},
		{"QU==QUJD", "A", ""},
		{"QUI==QU", "AB", ""},
		{"QU===J", "A", ""},
		{"QU=!=", "A", ""},
		{"QU=J=", "AB", ""},
		{"QU=JD", "ABC", ""},
		{"QUJD=QUJD", "ABCABC", ""},
		{"QU=JDQU=JD", "ABCABC", ""},
		{"Q", "", "Invalid base64-encoded string: number of data characters (1) cannot be 1 more than a multiple of 4"},
		{"Q===", "", "Invalid base64-encoded string: number of data characters (1) cannot be 1 more than a multiple of 4"},
		{"QUJDR", "", "Invalid base64-encoded string: number of data characters (5) cannot be 1 more than a multiple of 4"},
		{"QUJDREVGR", "", "Invalid base64-encoded string: number of data characters (9) cannot be 1 more than a multiple of 4"},
		{"QU", "", "Incorrect padding"},
		{"QUJ", "", "Incorrect padding"},
		{"QU=", "", "Incorrect padding"},
		{"QUJDé", "", "string argument should contain only ASCII characters"},
	} {
		got, err := decodeBase64(tc.data)
		switch {
		case tc.err != "" && (err == nil || err.Error() != tc.err):
			t.Errorf("%q: got %q, %v; want %q", tc.data, got, err, tc.err)
		case tc.err == "" && (err != nil || string(got) != tc.want):
			t.Errorf("%q: got %q, %v; want %q", tc.data, got, err, tc.want)
		}
	}
}
