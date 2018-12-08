package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	_ "strconv"
	"time"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"strconv"
)

const Name = "dynamic load generator"
const Version = "1.0"

type Config struct {
	Url                     string
	Clients                 int
	RequestsPerSecondTarget int
}

type Status struct {
	RequestPerSecondTarget  int
	RequestPerSecondCurrent int
}

var config = &Config{"", 0, 0}
var request = make(chan int)

var (
	requestCounter = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "requestCounter",
		Help: "Request counter"})

	requestDuration = prometheus.NewSummary(prometheus.SummaryOpts{
		Name:       "requestDuration",
		Help:       "Request duration",
		Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
		MaxAge:     15 * time.Second,
		AgeBuckets: 15,
	})

	runningClients = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "runningClients",
		Help: "Nr of running clients",
	})

	requestsPerSecondTarget = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "requestsPerSecondTarget",
		Help: "Nr of the target requests per second rate",
	})

	requestCodes = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "How many HTTP requests processed, partitioned by status code.",
		},
		[]string{"code"},
	)
)

func init() {
	// Register the summary and the histogram with Prometheus's default registry.
	prometheus.MustRegister(requestCounter)
	prometheus.MustRegister(requestDuration)
	prometheus.MustRegister(runningClients)
	prometheus.MustRegister(requestsPerSecondTarget)
	prometheus.MustRegister(requestCodes)

	flag.StringVar(&config.Url, "targetUrl", "http://pi-digits.aws-k8s.catalysts.cc/?digits=15000", "The url that should be load tested.")
	flag.IntVar(&config.Clients, "clients", 1, "Number of simulated clients.")
	flag.IntVar(&config.RequestsPerSecondTarget, "rps", 1, "Total number of requests per seconds the clients should try to make")
}

func main() {

	flag.Parse()

	fmt.Println(Name + " " + Version)

	go startClients()
	go makeRequests()

	r := mux.NewRouter()
	// Routes consist of a path and a handler function.
	r.Handle("/metrics", promhttp.Handler())
	r.HandleFunc("/api/status", statusHandler)
	r.HandleFunc("/api/config", getConfigHandler).Methods("GET")
	r.HandleFunc("/api/config", postConfigHandler).Methods("POST")
	r.PathPrefix("/").Handler(http.StripPrefix("/", http.FileServer(http.Dir("html/"))))

	// Bind to a port and pass our router in
	log.Fatal(http.ListenAndServe(":8080", r))
}

func startClients() {
	started := 0

	var stoppedChan = make(chan int)
	var killChan = make(chan int)
	for {
		if started < config.Clients {
			started++
			runningClients.Inc()
			go client(killChan, stoppedChan)
		}

		if started > config.Clients {
			killChan <- 1
			started--
			continue
		}

		select {
		case <-stoppedChan:
			runningClients.Dec()
		default:
		}

		time.Sleep(50 * time.Millisecond)
	}
}

func makeRequests() {
	requestNr := 0
	for {
		var rps = config.RequestsPerSecondTarget
		if rps > 0 {
			time.Sleep(time.Duration(int(time.Second) / config.RequestsPerSecondTarget))
			request <- requestNr
			requestNr++
		} else {
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func makeRequest(client *http.Client) (d time.Duration, statusCode int, err error) {
	started := time.Now()
	resp, err := client.Get(config.Url)

	if err != nil {
		// handle error
		return d, 0, err

	}
	io.Copy(ioutil.Discard, resp.Body)
	d = time.Now().Sub(started)

	defer resp.Body.Close()
	return d, resp.StatusCode, nil
}

func client(kill chan int, stopped chan int) {
	defer func() { stopped <- 0 }()

	httpClient := &http.Client{
		Transport: &http.Transport{
			MaxIdleConnsPerHost: 10,
		},
		Timeout: time.Duration(10) * time.Second,
	}

	for {
		select {
		case <-request:
			requestCounter.Inc()
			d, statusCode, err := makeRequest(httpClient)

			if err != nil {
				log.Printf("error making request: %s", err.Error())
			}
			requestCodes.WithLabelValues(strconv.Itoa(statusCode)).Inc()
			requestDuration.Observe(d.Seconds())
			log.Printf("Request took %v seconds", d.Seconds())
		case <-kill:
			return
		}
	}
}

func versionHandler(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte(Name + " " + Version))
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	var status Status
	outgoingJSON, error := json.Marshal(status)

	if error != nil {
		http.Error(w, error.Error(), http.StatusInternalServerError)
		return
	}

	fmt.Fprint(w, string(outgoingJSON))
}

func getConfigHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	outgoingJSON, error := json.Marshal(config)
	if error != nil {
		http.Error(w, error.Error(), http.StatusInternalServerError)
		return
	}
	fmt.Fprint(w, string(outgoingJSON))
}

func postConfigHandler(w http.ResponseWriter, r *http.Request) {
	var newConfig Config
	decoder := json.NewDecoder(r.Body)
	error := decoder.Decode(&newConfig)
	if error != nil {
		http.Error(w, error.Error(), http.StatusInternalServerError)
		return
	}
	config = &newConfig
	requestsPerSecondTarget.Set(float64(config.RequestsPerSecondTarget))
	getConfigHandler(w, r)
}
