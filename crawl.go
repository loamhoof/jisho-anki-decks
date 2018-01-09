package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/PuerkitoBio/goquery"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

var (
	stdout = log.New(os.Stdout, "", 0)
	stderr = log.New(os.Stderr, "", 0)
)

var (
	limits  = []int{33, 29, 89, 91 /*, 173*/}
	limitN1 = 15
)

var (
	rateLimiter = make(chan bool, 10)
)

func urlAPI(jlpt, page int) string {
	return fmt.Sprintf("http://jisho.org/api/v1/search/words?keyword=%%23jlpt-n%v&page=%v", jlpt, page)
}

func urlAPIN1(hiragana string, page int) string {
	return fmt.Sprintf("http://jisho.org/api/v1/search/words?keyword=%s%%20%%23jlpt-n1&page=%v", hiragana, page)
}

func urlAPICollocation(collocation string) string {
	return fmt.Sprintf("http://jisho.org/api/v1/search/words?keyword=%s", collocation)
}

func urlWordPage(word string) string {
	return fmt.Sprintf("http://jisho.org/word/%s", word)
}

func init() {
	for i := 0; i < 10; i++ {
		rateLimiter <- true
	}
}

func main() {
	wordsCh := make(chan *Word, 100)

	var wgAPI sync.WaitGroup

	for jlpt, pages := range limits {
		for page := 1; page <= pages; page++ {
			wgAPI.Add(1)

			go func(jlpt, page int) {
				defer wgAPI.Done()

				for _, word := range parseAPI(urlAPI(jlpt, page)) {
					wordsCh <- word
				}
			}(5-jlpt, page)
		}
	}

	for _, hiragana := range hiraganas {
		for page := 1; page <= limitN1; page++ {
			wgAPI.Add(1)

			go func(hiragana string, page int) {
				defer wgAPI.Done()

				for _, word := range parseAPI(urlAPIN1(hiragana, page)) {
					wordsCh <- word
				}
			}(hiragana, page)
		}
	}

	go func() {
		wgAPI.Wait()

		close(wordsCh)
	}()

	var wgWordPage sync.WaitGroup

	words := make(map[string]*Word)
	for word := range wordsCh {
		var key string
		if kanji := word.Japanese[0].Word; kanji != "" {
			key = kanji
		} else {
			key = word.Japanese[0].Reading
		}

		if _, ok := words[key]; ok {
			continue
		}

		words[key] = word

		wgWordPage.Add(1)

		go func(word *Word) {
			defer wgWordPage.Done()

			var ok bool

			word.JishoWordPage, word.Audios, word.Collocations, ok = findAndParseWordPage(word)
			if !ok {
				stderr.Printf("No word page found for: %s - %s", word.Japanese[0].Word, word.Japanese[0].Reading)

				return
			}

			if word.Audios != nil {
				downloadAudios(word.Audios)
			}

			if word.Collocations == nil {
				return
			}

			var wg sync.WaitGroup

			for i, collocation := range word.Collocations {
				wg.Add(1)

				go func(i int, collocation *Collocation) {
					defer wg.Done()

					kanji := parseCollocationSrc(collocation.Src)

					collocation.Word = parseAPI(urlAPICollocation(kanji))[0]

					var ok bool

					collocation.Word.JishoWordPage, collocation.Word.Audios, _, ok = findAndParseWordPage(collocation.Word)
					if !ok {
						collocation.Word.Audios, _, ok = parseWordPage(urlWordPage(kanji))

						if !ok {
							stderr.Printf("No word page found for: %s", collocation.Raw)

							return
						}

						collocation.Word.JishoWordPage = kanji
					}

					if collocation.Word.Audios != nil {
						downloadAudios(collocation.Word.Audios)
					}
				}(i, collocation)
			}

			wg.Wait()
		}(word)
	}

	wgWordPage.Wait()

	b, err := json.Marshal(words)
	if err != nil {
		panic(err)
	}

	stdout.Println(string(b))
}

func parseAPI(address string) []*Word {
	status, body, err := get(address)
	if err != nil {
		panic(err)
	}
	if status != http.StatusOK {
		panic(fmt.Sprintf("Status %v: %s", status, address))
	}
	defer body.Close()

	search := &Search{}
	if err := json.NewDecoder(body).Decode(search); err != nil {
		panic(err)
	}

	return search.Data
}

func findAndParseWordPage(word *Word) (string, Audios, Collocations, bool) {
	tried := make(map[string]bool)

	for _, jap := range word.Japanese {
		for _, reading := range []string{jap.Word, jap.Reading} {
			if _, ok := tried[reading]; ok || reading == "" {
				continue
			}
			tried[reading] = true

			address := urlWordPage(reading)

			if audios, collocations, ok := parseWordPage(address); ok {
				return reading, audios, collocations, true
			}
		}
	}

	return "", nil, nil, false
}

func parseWordPage(address string) (Audios, Collocations, bool) {
	status, body, err := get(address)
	if err != nil {
		panic(err)
	}
	if status == http.StatusNotFound {
		return nil, nil, false
	}
	if status != http.StatusOK {
		panic(fmt.Sprintf("Status %v: %s", status, address))
	}

	doc, err := goquery.NewDocumentFromReader(body)
	body.Close()
	if err != nil {
		panic(err)
	}

	var (
		audios       Audios
		collocations Collocations
	)
	nodes := doc.Find(".concept_light-status").Nodes

	for _, node := range nodes {
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			switch child.DataAtom {
			case atom.Audio:
				audios = getAudios(child)
			case atom.Div:
				collocations = getCollocations(child)
			}
		}
	}

	return audios, collocations, true
}

func getAudios(audioNode *html.Node) Audios {
	audios := make(Audios)

	for source := audioNode.FirstChild; source != nil; source = source.NextSibling {
		var src, _type string

		for _, attr := range source.Attr {
			switch attr.Key {
			case "src":
				src = attr.Val
			case "type":
				_type = attr.Val
			}
		}

		audios[_type] = &Audio{Src: src}
	}

	return audios
}

func getCollocations(div *html.Node) Collocations {
	collocations := make(Collocations, 0, 4)

	for li := div.FirstChild.FirstChild; li != nil; li = li.NextSibling {
		collocation := &Collocation{
			Raw: li.FirstChild.FirstChild.Data,
		}

		for _, attr := range li.FirstChild.Attr {
			if attr.Key != "href" {
				continue
			}

			collocation.Src = attr.Val

			break
		}

		collocations = append(collocations, collocation)
	}

	return collocations
}

func parseCollocationSrc(src string) string {
	search, _ := url.QueryUnescape(strings.TrimPrefix(src, "/search/"))

	return strings.TrimSpace(strings.TrimSuffix(search, "#words"))
}

func downloadAudios(audios Audios) {
	var wg sync.WaitGroup

	for _type, audio := range audios {
		wg.Add(1)

		go func(_type string, audio *Audio) {
			defer wg.Done()

			status, body, err := get(audio.Src)
			if err != nil {
				panic(err)
			}
			if status == http.StatusNotFound {
				delete(audios, _type)

				return
			}
			if status != http.StatusOK {
				panic(fmt.Sprintf("Status %v: %s", status, audio.Src))
			}
			defer body.Close()

			audio.Filename = filepath.Base(audio.Src)
			filename := filepath.Join("audio", audio.Filename)
			f, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.ModePerm)
			if err != nil {
				panic(err)
			}
			defer f.Close()

			if _, err := io.Copy(f, body); err != nil {
				panic(err)
			}
		}(_type, audio)
	}

	wg.Wait()

	return
}

func get(address string) (int, io.ReadCloser, error) {
	filename := filepath.Join("cache", url.PathEscape(address))

	if f, err := os.Open(filename); err == nil {
		return http.StatusOK, f, nil
	}

	<-rateLimiter
	defer func() {
		rateLimiter <- true
	}()

	res, err := http.Get(address)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()

	tmp, err := ioutil.TempFile("cache", "tmp_")
	if err != nil {
		return 0, nil, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	if res.StatusCode != http.StatusOK {
		return res.StatusCode, nil, nil
	}

	if _, err := io.Copy(tmp, res.Body); err != nil {
		return 0, nil, err
	}

	if err := os.Rename(tmp.Name(), filename); err != nil {
		return 0, nil, err
	}

	f, err := os.Open(filename)
	if err != nil {
		return 0, nil, err
	}

	return http.StatusOK, f, nil
}

type Search struct {
	Status int
	Data   []*Word
}

type Word struct {
	IsCommon bool     `json:"is_common"`
	Tags     []string `json:"tags"`
	Japanese []*struct {
		Word    string `json:"word"`
		Reading string `json:"reading"`
	} `json:"japanese"`
	Senses []*struct {
		EnglishDefinitions []string `json:"english_definitions"`
		PartsOfSpeech      []string `json:"parts_of_speech"`
		Links              []*struct {
			Text string `json:"text"`
			Url  string `json:"url"`
		} `json:"links"`
		Tags         []string `json:"tags"`
		Restrictions []string `json:"restrictions"`
		SeeAlso      []string `json:"see_also"`
		Antonyms     []string `json:"antonyms"`
		Source       []*struct {
			Language string `json:"language"`
			Word     string `json:"word"`
		} `json:"source"`
		Info []*string `json:"info"`
	} `json:"senses"`
	Attribution struct {
		Jmdict   bool        `json:"jmdict"`
		Jmnedict bool        `json:"jmnedict"`
		Dbpedia  interface{} `json:"dbpedia"`
	} `json:"attribution"`
	Collocations  Collocations `json:"collocations,omitempty"`
	Audios        Audios       `json:"audios,omitempty"`
	JishoWordPage string       `json:"jisho_word_page"`
}

type Collocations []*Collocation

type Collocation struct {
	Raw  string `json:"raw"`
	Src  string `json:"src"`
	Word *Word  `json:"word"`
}

type Audios map[string]*Audio

type Audio struct {
	Src      string `json:"src"`
	Filename string `json:"filename"`
}

var hiraganas = []string{"あ", "い", "う", "え", "お", "か", "が", "き", "ぎ", "く", "ぐ", "け", "げ", "こ", "ご", "さ", "ざ", "し", "じ", "す", "ず", "せ", "ぜ", "そ", "ぞ", "た", "だ", "ち", "ぢ", "つ", "づ", "て", "で", "と", "ど", "な", "に", "ぬ", "ね", "の", "は", "ば", "ぱ", "ひ", "び", "ぴ", "ふ", "ぶ", "ぷ", "へ", "べ", "ぺ", "ほ", "ぼ", "ぽ", "ま", "み", "む", "め", "も", "や", "ゆ", "よ", "ら", "り", "る", "れ", "ろ", "わ", "を", "ん"}
