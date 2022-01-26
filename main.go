package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/andygrunwald/go-jira"
	"github.com/go-redis/redis"
	"github.com/mmcdole/gofeed"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gopkg.in/yaml.v2"
	"jaytaylor.com/html2text"
)

var addr = flag.String("listen-address", ":8080", "The address to listen on for HTTP requests.")
var (
	lastRunGauge              prometheus.Gauge
	issuesCreatedCounter      prometheus.Counter
	issueCreationErrorCounter prometheus.Counter
)

type Config struct {
	Feeds    []Feed
	Interval int
}

type Feed struct {
	ID            string
	FeedURL       string `yaml:"feed_url"`
	Name          string
	JiraProjectID string `yaml:"jira_project_id"`
	Labels        []string
	AddedSince    time.Time `yaml:"added_since"`
}

type EnvValues struct {
	RedisURL      string
	RedisPassword string
	ConfDir       string
	JiraToken     string
	JiraUsername  string
	JiraURL       string
	UseSentinel   bool
	UseTLS        bool
}

func hasExistingJiraIssue(itemTitle string, projectKey string, jiraClient *jira.Client) bool {
	// Escape quotes in the title so its parsed correctly by Jira's JQL parser
	itemTitle = strings.ReplaceAll(itemTitle, `"`, `\"`)
	// Wrap the itemTitle in "\ \" so Jira does a direct match.
	//https://confluence.atlassian.com/jirasoftwareserver/search-syntax-for-text-fields-939938747.html
	jql := fmt.Sprintf("project = \"%s\" AND summary ~ \"\\\"%s\\\"\"", projectKey, itemTitle)
	log.Printf("Searching for existing issue \"%s\" in project %s\n", itemTitle, projectKey)
	issues, _, err := jiraClient.Issue.Search(jql, nil)
	if err != nil {
		log.Printf("Issue search failed for JQL: %s", jql)
		panic(err)
	}

	if len(issues) == 0 {
		return false
	} else if len(issues) > 1 {
		log.Printf("Found multiple issues that match \"%s\":", itemTitle)
		for _, x := range issues {
			log.Printf("%s ", x.Key)
		}
	}
	return true
}

func (feed Feed) checkFeed(redisClient *redis.Client, jiraClient *jira.Client) {
	fp := gofeed.NewParser()
	rss, err := fp.ParseURL(feed.FeedURL)

	if err != nil {
		log.Printf("Unable to parse feed %s: \n %s", feed.Name, err)
		return
	}

	var newArticle []*gofeed.Item
	var oldArticle []*gofeed.Item
	for _, item := range rss.Items {
		found := redisClient.SIsMember(feed.ID, item.GUID).Val()
		if found {
			oldArticle = append(oldArticle, item)
		} else {
			newArticle = append(newArticle, item)
		}
	}

	log.Printf("Checked feed: %s, New articles: %d, Old articles: %d", feed.Name, len(newArticle), len(oldArticle))

	for _, item := range newArticle {
		var itemTime time.Time
		// Prefer updated itemTime to published
		if item.UpdatedParsed != nil {
			itemTime = *item.UpdatedParsed
		} else {
			itemTime = *item.PublishedParsed
		}

		if itemTime.Before(feed.AddedSince) {
			log.Printf("Ignoring '%s' as its date is before the specified AddedSince (Item: %s vs AddedSince: %s)\n",
				item.Title, itemTime, feed.AddedSince)
			redisClient.SAdd(feed.ID, item.GUID)
			continue
		}

		// Check Jira to see if we already have a matching issue there
		if hasExistingJiraIssue(item.Title, feed.JiraProjectID, jiraClient) {
			// We think its new but there is already a matching Title in Jira.  Mark as Sync'd
			log.Printf("Adding \"%s\"to Redis as it was found in Jira\n", item.Title)
			redisClient.SAdd(feed.ID, item.GUID)
			continue
		}

		// Prefer description over content
		var body string
		if item.Description != "" {
			body = item.Description
		} else {
			body = item.Content
		}

		text, err := html2text.FromString(
			body, html2text.Options{PrettyTables: true, PrettyTablesOptions: html2text.NewPrettyTablesOptions()})
		if err != nil {
			log.Printf("Unable to parse HTML to text for \"%s\", falling back to HTML\n", item.Title)
			text = body
		}
		issue := jira.Issue{
			Fields: &jira.IssueFields{
				Type:        jira.IssueType{Name: "Task"},
				Project:     jira.Project{Key: feed.JiraProjectID},
				Description: text + "\n" + item.Link + "\n" + item.GUID,
				Summary:     item.Title,
				Labels:      feed.Labels,
			},
		}

		createdIssue, resp, err := jiraClient.Issue.Create(&issue)
		if err != nil {
			log.Printf("Unable to create Jira issue for %s \n %s \n", feed.Name, err)
			log.Print(resp)
			issueCreationErrorCounter.Inc()
			continue
		}

		if err := redisClient.SAdd(feed.ID, item.GUID).Err(); err != nil {
			log.Printf("Unable to persist in %s Redis: %s \n", item.Title, err)
			continue
		}

		fmt.Printf("%s: %+v\n", createdIssue.Key, createdIssue.Self)

		issuesCreatedCounter.Inc()
		log.Printf("Created Jira Issue '%s' in project: %s' \n", createdIssue.Key, feed.JiraProjectID)
	}
}

func readConfig(path string) *Config {
	config := &Config{}

	data, err := ioutil.ReadFile(path)
	if err != nil {
		log.Fatalln(err)
	}

	if err = yaml.Unmarshal(data, config); err != nil {
		log.Printf("Unable to parse config YAML \n %s \n", err)
		panic(err)
	}

	return config
}

func initialise(env EnvValues) (redisClient *redis.Client, jiraClient *jira.Client, config *Config) {
	gaugeOpts := prometheus.GaugeOpts{
		Name:      "last_run_time",
		Subsystem: "jira_rss_sync",
		Help:      "Last Run Time in Unix Seconds",
	}
	lastRunGauge = prometheus.NewGauge(gaugeOpts)
	prometheus.MustRegister(lastRunGauge)

	issuesCreatedCounterOpts := prometheus.CounterOpts{
		Name:      "issue_creation_total",
		Subsystem: "jira_rss_sync",
		Help:      "The total number of issues created in Jira since start-up",
	}
	issuesCreatedCounter = prometheus.NewCounter(issuesCreatedCounterOpts)
	prometheus.MustRegister(issuesCreatedCounter)

	issueCreationErrorCountOpts := prometheus.CounterOpts{
		Name:      "issue_creation_error_total",
		Subsystem: "jira_rss_sync",
		Help:      "The total of failures in creating Jira issues since start-up",
	}

	issueCreationErrorCounter = prometheus.NewCounter(issueCreationErrorCountOpts)
	prometheus.MustRegister(issueCreationErrorCounter)

	tp := jira.BasicAuthTransport{
		Username: env.JiraUsername,
		Password: env.JiraToken,
	}
	jiraClient, err := jira.NewClient(tp.Client(), env.JiraURL)
	if err != nil {
		log.Printf("Unable to authenticate with Jira: %s", err)
		panic(err)
	}

	config = readConfig(path.Join(env.ConfDir, "config.yaml"))

	if !env.UseSentinel {
		if env.UseTLS {
			log.Printf("TLS config enabled")
			redisClient = redis.NewClient(&redis.Options{
				Addr:      env.RedisURL,
				Password:  env.RedisPassword,
				DB:        0, // use default DB,
				TLSConfig: &tls.Config{},
			})
		} else {
			redisClient = redis.NewClient(&redis.Options{
				Addr:     env.RedisURL,
				Password: env.RedisPassword,
				DB:       0, // use default DB,
			})
		}
	} else {
		redisClient = redis.NewFailoverClient(&redis.FailoverOptions{
			SentinelAddrs: []string{env.RedisURL},
			Password:      env.RedisPassword,
			MasterName:    "mymaster",
			DB:            0, // use default DB
		})
	}

	if err := redisClient.Ping().Err(); err != nil {
		log.Printf("Unable to connect to Redis @ %s", env.RedisURL)
		log.Panicf(err.Error())
	} else {
		log.Printf("Connected to Redis @ %s", env.RedisURL)
	}

	return
}

func main() {
	env := readEnv()
	redisClient, jiraClient, config := initialise(env)
	go handleHTTP(redisClient)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			log.Printf("Running checks at %s\n", time.Now().Format(time.RFC850))
			for _, configEntry := range config.Feeds {
				configEntry.checkFeed(redisClient, jiraClient)
			}
			lastRunGauge.SetToCurrentTime()
			time.Sleep(time.Duration(config.Interval) * time.Second)
		}
	}()

	wg.Wait()
}

func readEnv() EnvValues {
	var jiraAPIToken, jiraURL, jiraUsername, configDir, redisURL, redisPassword string
	useSentinel := false

	if envJiraURL := os.Getenv("JIRA_URL"); envJiraURL == "" {
		panic("Could not find JIRA_URL specified as an environment variable")
	} else {
		jiraURL = envJiraURL
	}
	if envJiraUsername := os.Getenv("JIRA_USERNAME"); envJiraUsername == "" {
		panic("Could not find JIRA_USERNAME specified as an environment variable")
	} else {
		jiraUsername = envJiraUsername
	}
	if envJiraAPIToken := os.Getenv("JIRA_API_TOKEN"); envJiraAPIToken == "" {
		panic("Could not find JIRA_API_TOKEN specified as an environment variable")
	} else {
		jiraAPIToken = envJiraAPIToken
	}
	if envConfigDir := os.Getenv("CONFIG_DIR"); envConfigDir == "" {
		panic("Could not find CONFIG_DIR specified as an environment variable")
	} else {
		configDir = envConfigDir
	}

	envRedisPassword, hasRedisPasswordEnv := os.LookupEnv("REDIS_PASSWORD")
	envRedisAuthToken, hasRedisAuthTokenEnv := os.LookupEnv("REDIS_AUTH_TOKEN")
	if !hasRedisPasswordEnv && !hasRedisAuthTokenEnv {
		panic("Could not find REDIS_PASSWORD or REDIS_AUTH_TOKEN specified as an environment variable")
	} else {
		if envRedisPassword != "" {
			redisPassword = envRedisPassword
		} else {
			log.Printf("Using Redis auth token in place of password")
			redisPassword = envRedisAuthToken
		}
	}

	_, hasRedisSentinel := os.LookupEnv("USE_SENTINEL")
	if hasRedisSentinel {
		log.Printf("Running in sentinel aware mode")
		useSentinel = true
	}

	if envRedisURL := os.Getenv("REDIS_URL"); envRedisURL == "" {
		if envRedisEndpoint := os.Getenv("REDIS_PRIMARY_ENDPOINT"); envRedisEndpoint == "" {
			panic("Could not find REDIS_URL or REDIS_PRIMARY_ENDPOINT specified as an environment variable")
		} else {
			log.Printf("Using Redis primary endpoint as URL")
			redisURL = fmt.Sprintf("%s:6379", envRedisEndpoint)
		}
	} else {
		redisURL = envRedisURL
	}

	useTLS := false
	if envUseTLS := os.Getenv("REDIS_SSL"); envUseTLS == "1" {
		useTLS = true
	} else {
		useTLS = false
	}

	return EnvValues{
		RedisURL:      redisURL,
		RedisPassword: redisPassword,
		ConfDir:       configDir,
		JiraToken:     jiraAPIToken,
		JiraUsername:  jiraUsername,
		JiraURL:       jiraURL,
		UseSentinel:   useSentinel,
		UseTLS:        useTLS,
	}
}

func handleHTTP(client *redis.Client) {
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := client.Ping().Err(); err != nil {
			http.Error(w, "Unable to connect to the redis master", http.StatusInternalServerError)
		} else {
			fmt.Fprintf(w, "All is well!")
		}
	})

	http.Handle("/metrics", promhttp.Handler())
	log.Fatal(http.ListenAndServe(*addr, nil))
}
