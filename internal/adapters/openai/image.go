package openai

import (
	"fmt"
	"strings"

	"github.com/sarpaydemir/melinda/internal/port/model"
	"github.com/sarpaydemir/melinda/internal/textfmt"
)

// imageError is an image failure in the established wording the model reads.
type imageError string

func (e imageError) Error() string { return string(e) }

// image is the first base64 data URL among an answer's image URLs, decoded. Its media type is image/png when the
// URL's header mentions png, and image/jpeg otherwise.
func image(answer []byte) (model.Image, error) {
	v, err := textfmt.Decode(answer, true)
	if err != nil {
		return model.Image{}, err
	}
	for _, url := range imageURLs(v) {
		s, ok := url.(string)
		if !ok || !strings.HasPrefix(s, "data:image/") {
			continue
		}
		header, data, ok := strings.Cut(s, ",")
		if !ok {
			continue
		}
		decoded, err := decodeBase64(data)
		if err != nil {
			return model.Image{}, err
		}
		mediaType := "image/jpeg"
		if strings.Contains(header, "png") {
			mediaType = "image/png"
		}
		return model.Image{MediaType: mediaType, Data: decoded}, nil
	}
	return model.Image{}, imageError("No image in response")
}

// imageURLs are the image URLs of an answer's first message: those of its images, then those of its content's
// image parts. A member of an unexpected type counts as absent.
func imageURLs(answer any) []any {
	choices, _ := member(answer, "choices").([]any)
	if len(choices) == 0 {
		return nil
	}
	message := member(choices[0], "message")
	var urls []any
	images, _ := member(message, "images").([]any)
	for _, img := range images {
		if s, ok := img.(string); ok {
			urls = append(urls, s)
			continue
		}
		url := member(member(img, "image_url"), "url")
		if !textfmt.Truthy(url) {
			url = member(img, "url")
		}
		urls = append(urls, url)
	}
	parts, _ := member(message, "content").([]any)
	for _, part := range parts {
		if member(part, "type") == "image_url" {
			urls = append(urls, member(member(part, "image_url"), "url"))
		}
	}
	return urls
}

// member is the member key of v when v is an object, and nil otherwise.
func member(v any, key string) any {
	o, _ := v.(textfmt.Object)
	return o.Get(key)
}

// base64Values maps each byte to its value in the standard base64 alphabet, or 0xff.
var base64Values = func() (values [256]byte) {
	for i := range values {
		values[i] = 0xff
	}
	for i, c := range "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/" {
		values[c] = byte(i)
	}
	return values
}()

// decodeBase64 decodes data leniently: characters outside the alphabet are skipped, padding counts only after the
// first two characters of a quantum, and decoding stops at the padding that completes a quantum.
func decodeBase64(data string) ([]byte, error) {
	for i := range len(data) {
		if data[i] >= 0x80 {
			return nil, imageError("string argument should contain only ASCII characters")
		}
	}
	out := make([]byte, 0, len(data)/4*3+2)
	var quantum, pads int
	var left byte
	for i := range len(data) {
		if data[i] == '=' {
			if quantum >= 2 {
				pads++
				if quantum+pads >= 4 {
					return out, nil
				}
			}
			continue
		}
		v := base64Values[data[i]]
		if v == 0xff {
			continue
		}
		pads = 0
		switch quantum {
		case 0:
			left = v
		case 1:
			out = append(out, left<<2|v>>4)
			left = v & 0x0f
		case 2:
			out = append(out, left<<4|v>>2)
			left = v & 0x03
		case 3:
			out = append(out, left<<6|v)
		}
		quantum = (quantum + 1) % 4
	}
	switch quantum {
	case 0:
		return out, nil
	case 1:
		return nil, imageError(fmt.Sprintf("Invalid base64-encoded string: number of data characters (%d) cannot be 1 more than a multiple of 4", len(out)/3*4+1))
	}
	return nil, imageError("Incorrect padding")
}
