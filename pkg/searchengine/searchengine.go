package searchengine

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/sirupsen/logrus"
	"io"
	"log"
	"net/url"
	"regexp"
	"strings"
	"time"
	"url-collector/config"
	"url-collector/pkg/alg"
	"url-collector/pkg/filter"
	"url-collector/pkg/request"

	mapset "github.com/deckarep/golang-set"
)

func newSearchEngine(config SearchEngineConfig) *SearchEngine {
	ctx, cancel := context.WithCancel(context.Background())
	return &SearchEngine{
		SearchEngineConfig: config,
		atagRe:             regexp.MustCompile(`<a[^>]+href="(http[^>"]+)"[^>]+>`),
		dorkCh:             make(chan string, 10240),
		resultCh:           make(chan string, 1024),
		ctx:                ctx,
		cancel:             cancel,
		progress:           alg.NewProgress(),
		FinishedDorkSet:    mapset.NewSet(),
	}
}

// NewBing Bing搜索
func NewBing() *SearchEngine {
	return newSearchEngine(SearchEngineConfig{
		baseURL:    config.CurrentConf.GetBaseURL(),
		nextPageRe: regexp.MustCompile(`<a[^>]+href="(/search\?q=[^>]+)"[^>]+>`),
		userAgent:  "Mozilla/5.0 (iPhone; CPU iPhone OS 11_0 like Mac OS X) AppleWebKit/604.1.38 (KHTML, like Gecko) Version/11.0 Mobile/15A372 Safari/604.1",
	})
}

// NewGoogleImage Google 镜像搜索
func NewGoogleImage(conf BaseConfig) *SearchEngine {
	return newSearchEngine(SearchEngineConfig{
		BaseConfig: conf,
		baseURL:    config.CurrentConf.GetBaseURL(),
		userAgent:  "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:92.0) Gecko/20100101 Firefox/92.0",
		nextPageRe: regexp.MustCompile(`<a href="(/search\?q=[^>]+)" id="pnnext"[^>]+>`),
	})
}

// NewBaidu Baidu 搜索
func NewBaidu(conf BaseConfig) *SearchEngine {
	return newSearchEngine(SearchEngineConfig{
		BaseConfig: conf,
		baseURL:    config.CurrentConf.GetBaseURL(),
		userAgent:  "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:92.0) Gecko/20100101 Firefox/92.0",
		nextPageRe: regexp.MustCompile(`<a class="n" href="(/s\?wd=[^>]+)">下一页 &gt;</a>`),
	})
}

// NewGoogle Google搜索
func NewGoogle(conf BaseConfig) *SearchEngine {
	return newSearchEngine(SearchEngineConfig{
		BaseConfig: conf,
		baseURL:    config.CurrentConf.GetBaseURL(),
		nextPageRe: regexp.MustCompile(`<a href="(/search\?q=[^>]+)" id="pnnext"[^>]+>`),
		userAgent:  "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:92.0) Gecko/20100101 Firefox/92.0",
	})

}

func (s *SearchEngine) save() {
	s.saverWg.Add(1)
	go func() {
		for result := range s.resultCh {
			// 解析结构体
			var data SearchResultJson
			_ = json.Unmarshal([]byte(result), &data)

			time.Sleep(time.Millisecond * 100)
			//0.格式化结果
			r, err := s.formatResult(data.Url)
			if err != nil {
				log.Println("s.formatResult failed,err:", err)
				continue
			}
			//1.过滤重复的
			if val, err := filter.URLFilter.IsDuplicate(r); err != nil || val {
				continue
			}
			//2.过滤黑名单中的
			if filter.URLFilter.IsInBlackList(r) {
				continue
			}
			_, _ = fmt.Fprintln(s.ResultWriter, fmt.Sprintf("%s,\t%s", data.Keyword, data.Url))
		}
		s.saverWg.Done()
	}()
}

// 根据配置项格式化采集结果
func (s *SearchEngine) formatResult(rawurl string) (string, error) {
	URL, err := url.Parse(rawurl)
	if err != nil {
		return "", err
	}
	switch s.Format {
	case "domain":
		return URL.Host, nil
	case "url":
		return strings.ReplaceAll(rawurl, "&amp;", "&"), nil
	case "protocol_domain":
		return URL.Scheme + "://" + URL.Host, nil
	}
	return rawurl, nil
}

// 采集url
func (s *SearchEngine) fetch() {
	for i := 0; i < config.CurrentConf.RoutineCount; i++ {
		s.fetcherWg.Add(1)
		go func() {
		LOOP:
			for {
				select {
				case <-s.ctx.Done():
					break LOOP
				case dork := <-s.dorkCh:
					headers := map[string]string{
						"User-Agent":    s.userAgent,
						"X-Forward-For": "genIP()",
					}
					//1.发送请求
					resp, err := request.Get(dork, headers)
					if err != nil {
						logrus.Printf("requests.Get(%s) failed,err:%v", dork, err)
						continue
					}
					if resp.StatusCode > 300 && resp.StatusCode < 400 {
						s.dorkCh <- resp.Header.Get("Location")
					}
					defer func(Body io.ReadCloser) {
						err := Body.Close()
						if err != nil {
							logrus.Printf("Body.Close() failed,err:%v", err)
						}
					}(resp.Body)
					bytes, err := io.ReadAll(resp.Body)
					if err != nil {
						log.Printf("io.ReadAll failed,err:%v", err)
						continue
					}
					text := string(bytes)
					//2.解析响应 寻找指定文件的URL
					if strings.Contains(text, "window.location.href") {
						s.dorkCh <- dork
						continue
					}
					if strings.Contains(text, "网络不给力，请稍后重试") {
						s.dorkCh <- dork
						continue
					}
					matches := s.atagRe.FindAllStringSubmatch(text, -1)
					for _, match := range matches {
						link := match[1]
						//0.检查重定向
						urlString, err := filter.URLFilter.CheckRedirect(link)
						if err != nil {
							log.Println("filter.URLFilter.CheckRedirect failed,,err:", err)
							continue
						}
						// 获取keyword
						queryCore, _ := url.Parse(dork)
						var keyword string
						if len(queryCore.Query().Get("wd")) == 0 {
							keyword = queryCore.Query().Get("q")
						} else {
							keyword = queryCore.Query().Get("wd")
						}
						// 序列化存储
						resultStruct := SearchResultJson{
							Keyword: keyword,
							Url:     urlString,
						}
						resultString, _ := json.Marshal(resultStruct)
						s.resultCh <- string(resultString)
					}
					//3.寻找“下一页URL”
					u, err := url.Parse(dork)
					if err != nil {
						log.Println("url.Parse failed,err:", err)
						return
					}
					keyword := u.Query().Get("q")
					nextPageURLs := make([]string, 0)
					matches = s.nextPageRe.FindAllStringSubmatch(text, -1)
					for _, match := range matches {
						nextPageURL := strings.ReplaceAll(u.Scheme+"://"+u.Host+match[1], "&amp;", "&")
						nextPageURLs = append(nextPageURLs, nextPageURL)
					}
					if len(nextPageURLs) > 0 {
						for i := range nextPageURLs {
							s.dorkCh <- nextPageURLs[i]
						}
					} else {
						if s.FinishedDorkSet.Contains(keyword) {
							continue
						}
						s.FinishedDorkSet.Add(keyword)
						s.dorkWg.Done() //针对该dork的搜索任务完成
						s.progress.AddFinished()
					}
				}
			}
			s.fetcherWg.Done()
		}()
	}
}

// Search 开始搜索
func (s *SearchEngine) Search() {
	//定时显示进度
	s.progress.Show(s.ctx)
	//保存结果  (消费者:快)
	s.save()
	//发送请求 （生产者:慢）
	s.fetch()
	//从reader中读取dork
	scanner := bufio.NewScanner(s.DorkReader)
	for scanner.Scan() {
		keyword := strings.TrimSpace(scanner.Text())
		req := strings.ReplaceAll(s.baseURL, "$keyword", keyword)
		s.dorkCh <- req
		s.dorkWg.Add(1)
		s.progress.AddTotal()
	}
	//等待各部门结束工作
	s.wait()
}

func (s *SearchEngine) wait() {
	//因为dork是有限的，所以等待所有dork搜索完成
	s.dorkWg.Wait()
	//通知fetcher和progress结束工作
	s.cancel()
	s.fetcherWg.Wait()
	//关闭resultCh
	close(s.resultCh)
	//等待saver结束工作
	s.saverWg.Wait()
	fmt.Println("\n搜索完成")
}
