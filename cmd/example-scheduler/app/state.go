package app

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	proto "github.com/gogo/protobuf/proto"
	"github.com/mesos/mesos-go"
	"github.com/mesos/mesos-go/backoff"
	"github.com/mesos/mesos-go/httpcli"
	"github.com/mesos/mesos-go/httpcli/httpsched"
)

func prepareExecutorInfo(
	execBinary, execImage string,
	server server,
	wantsResources mesos.Resources,
	jobRestartDelay time.Duration,
	metricsAPI *metricsAPI,
) (*mesos.ExecutorInfo, error) {
	if execImage != "" {
		// Create mesos custom executor
		return &mesos.ExecutorInfo{
			ExecutorID: mesos.ExecutorID{Value: "default"},
			Name:       proto.String("Test Executor"),
			Command: mesos.CommandInfo{
				Shell: func() *bool { x := false; return &x }(),
			},
			Container: &mesos.ContainerInfo{
				Type: mesos.ContainerInfo_DOCKER.Enum(),
				Docker: &mesos.ContainerInfo_DockerInfo{
					Image:          execImage,
					ForcePullImage: func() *bool { x := true; return &x }(),
					Parameters: []mesos.Parameter{
						{
							Key:   "entrypoint",
							Value: execBinary,
						}}}},
			Resources: wantsResources,
		}, nil
	} else if execBinary != "" {
		log.Println("No executor image specified, will serve executor binary from built-in HTTP server")
		listener, iport, err := newListener(server)
		if err != nil {
			return nil, err
		}
		server.port = iport // we're just working with a copy of server, so this is OK
		var (
			mux                    = http.NewServeMux()
			executorUris           = []mesos.CommandInfo_URI{}
			uri, executorCmd, err2 = serveExecutorArtifact(server, execBinary, mux)
			executorCommand        = fmt.Sprintf("./%s", executorCmd)
		)
		if err2 != nil {
			return nil, err2
		}
		wrapper := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			metricsAPI.artifactDownloads()
			mux.ServeHTTP(w, r)
		})
		executorUris = append(executorUris, mesos.CommandInfo_URI{Value: uri, Executable: proto.Bool(true)})

		go forever("artifact-server", jobRestartDelay, metricsAPI.jobStartCount, func() error { return http.Serve(listener, wrapper) })
		log.Println("Serving executor artifacts...")

		// Create mesos custom executor
		return &mesos.ExecutorInfo{
			ExecutorID: mesos.ExecutorID{Value: "default"},
			Name:       proto.String("Test Executor"),
			Command: mesos.CommandInfo{
				Value: proto.String(executorCommand),
				URIs:  executorUris,
			},
			Resources: wantsResources,
		}, nil
	}
	return nil, errors.New("must specify an executor binary or image")
}

func buildWantsTaskResources(config Config) (r mesos.Resources) {
	r.Add(
		*mesos.BuildResource().Name("cpus").Scalar(config.taskCPU).Resource,
		*mesos.BuildResource().Name("mem").Scalar(config.taskMemory).Resource,
	)
	log.Println("wants-task-resources = " + r.String())
	return
}

func buildWantsExecutorResources(config Config) (r mesos.Resources) {
	r.Add(
		*mesos.BuildResource().Name("cpus").Scalar(config.execCPU).Resource,
		*mesos.BuildResource().Name("mem").Scalar(config.execMemory).Resource,
	)
	log.Println("wants-executor-resources = " + r.String())
	return
}

func buildHTTPClient(cfg Config) httpsched.Client {
	cli := httpcli.New(
		httpcli.Endpoint(cfg.url),
		httpcli.Codec(cfg.codec.Codec),
		httpcli.Do(httpcli.With(httpcli.Timeout(cfg.timeout))),
	)
	if cfg.compression {
		// TODO(jdef) experimental; currently released versions of Mesos will accept this
		// header but will not send back compressed data due to flushing issues.
		log.Println("compression enabled")
		cli.With(httpcli.RequestOptions(httpcli.Header("Accept-Encoding", "gzip")))
	}
	return httpsched.NewClient(cli)
}

func buildFrameworkInfo(cfg Config) *mesos.FrameworkInfo {
	frameworkInfo := &mesos.FrameworkInfo{
		User:       cfg.user,
		Name:       cfg.name,
		Checkpoint: &cfg.checkpoint,
	}
	if cfg.role != "" {
		frameworkInfo.Role = &cfg.role
	}
	if cfg.principal != "" {
		frameworkInfo.Principal = &cfg.principal
	}
	if cfg.hostname != "" {
		frameworkInfo.Hostname = &cfg.hostname
	}
	if len(cfg.labels) > 0 {
		log.Println("using labels:", cfg.labels)
		frameworkInfo.Labels = &mesos.Labels{Labels: cfg.labels}
	}
	return frameworkInfo
}

func newInternalState(cfg Config) (*internalState, error) {
	metricsAPI := initMetrics(cfg)
	executorInfo, err := prepareExecutorInfo(
		cfg.executor,
		cfg.execImage,
		cfg.server,
		buildWantsExecutorResources(cfg),
		cfg.jobRestartDelay,
		metricsAPI,
	)
	if err != nil {
		return nil, err
	}
	state := &internalState{
		config:             cfg,
		totalTasks:         cfg.tasks,
		reviveTokens:       backoff.BurstNotifier(cfg.reviveBurst, cfg.reviveWait, cfg.reviveWait, nil),
		wantsTaskResources: buildWantsTaskResources(cfg),
		executor:           executorInfo,
		metricsAPI:         metricsAPI,
		cli:                buildHTTPClient(cfg),
	}
	return state, nil
}

type internalState struct {
	tasksLaunched      int
	tasksFinished      int
	totalTasks         int
	frameworkID        string
	role               string
	executor           *mesos.ExecutorInfo
	cli                httpsched.Caller
	config             Config
	wantsTaskResources mesos.Resources
	reviveTokens       <-chan struct{}
	metricsAPI         *metricsAPI
	err                error
	done               bool
}