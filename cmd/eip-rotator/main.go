package main

import (
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/ucloud/ucloud-sdk-go/services/uaccount"
	"github.com/ucloud/ucloud-sdk-go/services/unet"
	"github.com/ucloud/ucloud-sdk-go/ucloud"
	"github.com/ucloud/ucloud-sdk-go/ucloud/auth"
	ucfg "github.com/ucloud/ucloud-sdk-go/ucloud/config"
)

type runner struct {
	cancel context.CancelFunc
	cfg    taskConfig
}

type taskConfig struct {
	PublicKey  string   `json:"public_key"`
	PrivateKey string   `json:"private_key"`
	Projects   []string `json:"project_ids"`
	Region     string   `json:"region"`
	Interval   int      `json:"interval_sec"`
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("eip-rotator ")

	var (
		mode       string
		publicKey  string
		privateKey string
		projectIDs string
		region     string
		interval   int
		configPath string
	)

	flag.StringVar(&mode, "mode", "run", "mode: run|schedule")
	flag.StringVar(&publicKey, "public-key", os.Getenv("UCLOUD_PUBLIC_KEY"), "ucloud public key")
	flag.StringVar(&privateKey, "private-key", os.Getenv("UCLOUD_PRIVATE_KEY"), "ucloud private key")
	flag.StringVar(&projectIDs, "project-ids", os.Getenv("UCLOUD_PROJECT_IDS"), "comma-separated project ids")
	flag.StringVar(&region, "region", os.Getenv("UCLOUD_REGION"), "ucloud region, e.g. cn-bj2")
	flag.IntVar(&interval, "interval", 300, "interval seconds to rotate eip")
	flag.StringVar(&configPath, "config", "", "json config file for task list")
	flag.Parse()

	switch mode {
	case "run":
		if configPath != "" {
			runFromConfig(configPath)
			return
		}
		if publicKey == "" || privateKey == "" || projectIDs == "" {
			log.Fatal("missing required flags: --public-key, --private-key, --project-ids")
		}
		projects := strings.Split(projectIDs, ",")
		cfg := taskConfig{PublicKey: publicKey, PrivateKey: privateKey, Projects: projects, Region: region, Interval: interval}
		if err := rotateOnce(cfg); err != nil {
			log.Fatalf("rotate failed: %v", err)
		}
	case "schedule":
		if configPath == "" {
			log.Fatal("--config is required in schedule mode (supports multi-task)")
		}
		runScheduler(configPath)
	default:
		log.Fatalf("unknown mode: %s", mode)
	}
}

func runFromConfig(path string) {
	f, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read config: %v", err)
	}
	var tasks []taskConfig
	if err := json.Unmarshal(f, &tasks); err != nil {
		log.Fatalf("parse config: %v", err)
	}
	for _, t := range tasks {
		if t.PublicKey == "" || t.PrivateKey == "" || len(t.Projects) == 0 {
			log.Fatalf("invalid task config: %+v", t)
		}
		if t.Interval <= 0 {
			t.Interval = 300
		}
		if err := rotateOnce(t); err != nil {
			log.Printf("task failed (region=%s, projects=%v): %v", t.Region, t.Projects, err)
		}
	}
}

// rotateOnce implements: list bound EIPs -> allocate new EIPs with same spec -> unbind old -> bind new
func rotateOnce(task taskConfig) error {
	credential := auth.NewCredential()
	credential.PublicKey = task.PublicKey
	credential.PrivateKey = task.PrivateKey

	// resolve regions: explicit or all accessible
	regions := []string{}
	if strings.TrimSpace(task.Region) == "" {
		rgs, err := listAccessibleRegions(&credential)
		if err != nil {
			return fmt.Errorf("list regions: %w", err)
		}
		regions = rgs
	} else {
		regions = []string{task.Region}
	}

	var firstErr error
	for _, region := range regions {
		if err := rotateOnceForRegion(task, &credential, region); err != nil {
			if firstErr == nil {
				firstErr = err
			} else {
				log.Printf("warn: region %s failed: %v", region, err)
			}
		}
	}
	return firstErr
}

func rotateOnceForRegion(task taskConfig, credential *auth.Credential, region string) error {
	baseCfg := ucfg.NewConfig()
	baseCfg.Region = region
	cfg := &ucloud.Config{ // alias type; take address for client constructors
		Region:     baseCfg.Region,
		Zone:       baseCfg.Zone,
		ProjectId:  baseCfg.ProjectId,
		BaseUrl:    baseCfg.BaseUrl,
		UserAgent:  baseCfg.UserAgent,
		Timeout:    baseCfg.Timeout,
		MaxRetries: baseCfg.MaxRetries,
		LogLevel:   baseCfg.LogLevel,
	}

	unetClient := unet.NewClient(cfg, credential)

	// Step 1: list all uhosts with bound eip per project
	type hostBinding struct {
		ProjectID     string
		UHostID       string
		UHostName     string
		EIPID         string
		EIPBandwidth  int
		EIPPayMode    string
		EIPOperator   string
		EIPChargeType string
		Region        string
	}
	var bindings []hostBinding

	for _, project := range task.Projects {
		// DescribeEIP and filter: Status==used and Resource.ResourceType==uhost
		deReq := unetClient.NewDescribeEIPRequest()
		deReq.ProjectId = ucloud.String(project)
		// leave default filters; we will filter by ResourceType later
		deResp, err := unetClient.DescribeEIP(deReq)
		if err != nil {
			return fmt.Errorf("DescribeEIP: %w", err)
		}
		for _, e := range deResp.EIPSet {
			if strings.ToLower(e.Status) != "used" {
				continue
			}
			if strings.ToLower(e.Resource.ResourceType) != "uhost" {
				continue
			}
			if e.Resource.ResourceID == "" {
				continue
			}
			bw := e.Bandwidth
			op := ""
			if len(e.EIPAddr) > 0 {
				op = e.EIPAddr[0].OperatorName
			}
			pay := e.PayMode
			charge := e.ChargeType
			bindings = append(bindings, hostBinding{
				ProjectID:     project,
				UHostID:       e.Resource.ResourceID,
				UHostName:     e.Resource.ResourceName,
				EIPID:         e.EIPId,
				EIPBandwidth:  bw,
				EIPPayMode:    pay,
				EIPOperator:   op,
				EIPChargeType: charge,
				Region:        region,
			})
		}
	}

	if len(bindings) == 0 {
		return errors.New("no bound EIP found under given projects")
	}

	// Step 2/3: for each host, allocate new eip with same spec, then switch
	for _, b := range bindings {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()

		// Allocate new EIP
		allocReq := unetClient.NewAllocateEIPRequest()
		allocReq.ProjectId = ucloud.String(b.ProjectID)
		allocReq.OperatorName = ucloud.String(b.EIPOperator)
		allocReq.Bandwidth = ucloud.Int(b.EIPBandwidth)
		allocReq.PayMode = ucloud.String(b.EIPPayMode)
		allocReq.ChargeType = ucloud.String(b.EIPChargeType)
		// 对于按年/按月付费，设置购买时长为1（1年或1个月）
		if b.EIPChargeType == "Year" || b.EIPChargeType == "Month" {
			allocReq.Quantity = ucloud.Int(1)
		}
		// 计费方式已设置为与旧EIP一致：ChargeType(付费方式)和PayMode(计费模式)
		allocResp, err := unetClient.AllocateEIP(allocReq)
		if err != nil {
			return fmt.Errorf("AllocateEIP: region=%s host=%s(%s): %w", b.Region, safeName(b.UHostName), b.UHostID, err)
		}
		if len(allocResp.EIPSet) == 0 {
			return fmt.Errorf("AllocateEIP returned empty set: region=%s host=%s(%s)", b.Region, safeName(b.UHostName), b.UHostID)
		}
		newEipID := allocResp.EIPSet[0].EIPId

		// Unbind old EIP
		unbindReq := unetClient.NewUnBindEIPRequest()
		unbindReq.ProjectId = ucloud.String(b.ProjectID)
		unbindReq.EIPId = ucloud.String(b.EIPID)
		unbindReq.ResourceId = ucloud.String(b.UHostID)
		unbindReq.ResourceType = ucloud.String("uhost")
		if _, err := unetClient.UnBindEIP(unbindReq); err != nil {
			return fmt.Errorf("UnBindEIP: region=%s host=%s(%s): %w", b.Region, safeName(b.UHostName), b.UHostID, err)
		}

		// Bind new EIP
		bindReq := unetClient.NewBindEIPRequest()
		bindReq.ProjectId = ucloud.String(b.ProjectID)
		bindReq.EIPId = ucloud.String(newEipID)
		bindReq.ResourceType = ucloud.String("uhost")
		bindReq.ResourceId = ucloud.String(b.UHostID)
		if _, err := unetClient.BindEIP(bindReq); err != nil {
			return fmt.Errorf("BindEIP: region=%s host=%s(%s): %w", b.Region, safeName(b.UHostName), b.UHostID, err)
		}

		// Optional: release old EIP after switch to avoid leak
		relReq := unetClient.NewReleaseEIPRequest()
		relReq.ProjectId = ucloud.String(b.ProjectID)
		relReq.EIPId = ucloud.String(b.EIPID)
		if _, err := unetClient.ReleaseEIP(relReq); err != nil {
			log.Printf("warn: region=%s host=%s(%s) ReleaseEIP failed for %s: %v", b.Region, safeName(b.UHostName), b.UHostID, b.EIPID, err)
		}

		log.Printf("rotated EIP for region=%s host=%s(%s) old=%s new=%s", b.Region, safeName(b.UHostName), b.UHostID, b.EIPID, newEipID)
		_ = ctx
	}

	return nil
}

func listAccessibleRegions(credential *auth.Credential) ([]string, error) {
	cfg := ucfg.NewConfig() // Region empty for account-wide
	uacct := uaccount.NewClient(&ucloud.Config{Region: cfg.Region, Zone: cfg.Zone, ProjectId: cfg.ProjectId, BaseUrl: cfg.BaseUrl, UserAgent: cfg.UserAgent, Timeout: cfg.Timeout, MaxRetries: cfg.MaxRetries, LogLevel: cfg.LogLevel}, credential)
	req := uacct.NewGetRegionRequest()
	resp, err := uacct.GetRegion(req)
	if err != nil {
		return nil, err
	}
	regions := make([]string, 0, len(resp.Regions))
	uniq := map[string]struct{}{}
	for _, r := range resp.Regions {
		if _, ok := uniq[r.Region]; ok {
			continue
		}
		uniq[r.Region] = struct{}{}
		regions = append(regions, r.Region)
	}

	return regions, nil
}

func safeName(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// runScheduler: in-process seconds-level scheduler with config hot-reload
func runScheduler(configPath string) {
	logger := log.New(os.Stdout, "scheduler ", log.LstdFlags|log.Lmsgprefix)

	// runner type is declared at package scope

	keyOf := func(t taskConfig) string {
		key := fmt.Sprintf("%s|%s|%s", strings.TrimSpace(t.PublicKey), strings.TrimSpace(t.PrivateKey), strings.Join(t.Projects, ","))
		return fmt.Sprintf("%x", sha1Bytes([]byte(key)))
	}

	active := map[string]runner{}

	reconcile := func(tasks []taskConfig) {
		seen := map[string]bool{}
		for _, t := range tasks {
			if t.Interval <= 0 {
				t.Interval = 300
			}
			k := keyOf(t)
			seen[k] = true
			if r, ok := active[k]; ok {
				if r.cfg.Region != t.Region || r.cfg.Interval != t.Interval {
					r.cancel()
					delete(active, k)
					start := startTask(t, logger)
					active[k] = start
					logger.Printf("updated task key=%s region=%s interval=%ds", k, t.Region, t.Interval)
				}
				continue
			}
			start := startTask(t, logger)
			active[k] = start
			logger.Printf("started task key=%s region=%s interval=%ds", k, t.Region, t.Interval)
		}
		for k, r := range active {
			if !seen[k] {
				r.cancel()
				delete(active, k)
				logger.Printf("stopped task key=%s", k)
			}
		}
	}

	load := func() []taskConfig {
		b, err := os.ReadFile(configPath)
		if err != nil {
			logger.Fatalf("read config: %v", err)
		}
		var tasks []taskConfig
		if err := json.Unmarshal(b, &tasks); err != nil {
			logger.Fatalf("parse config: %v", err)
		}
		if len(tasks) == 0 {
			logger.Fatalf("empty tasks in config")
		}
		for _, t := range tasks {
			if t.PublicKey == "" || t.PrivateKey == "" || len(t.Projects) == 0 {
				logger.Fatalf("invalid task: %+v", t)
			}
		}
		return tasks
	}

	tasks := load()
	reconcile(tasks)

	var lastMod time.Time
	if fi, err := os.Stat(configPath); err == nil {
		lastMod = fi.ModTime()
	}
	for {
		time.Sleep(5 * time.Second)
		fi, err := os.Stat(configPath)
		if err != nil {
			continue
		}
		if fi.ModTime().After(lastMod) {
			lastMod = fi.ModTime()
			logger.Printf("detected config update, reloading")
			tasks = load()
			reconcile(tasks)
		}
	}
}

func startTask(t taskConfig, logger *log.Logger) runner {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		logger.Printf("task run start: region=%s interval=%ds projects=%v", t.Region, t.Interval, t.Projects)
		start := time.Now()
		err := rotateOnce(t)
		dur := time.Since(start)
		if err != nil {
			logger.Printf("task run end: region=%s interval=%ds took=%s error=%v", t.Region, t.Interval, dur, err)
		} else {
			logger.Printf("task run end: region=%s interval=%ds took=%s", t.Region, t.Interval, dur)
		}
		ticker := time.NewTicker(time.Duration(t.Interval) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				logger.Printf("task run start: region=%s interval=%ds projects=%v", t.Region, t.Interval, t.Projects)
				start := time.Now()
				err := rotateOnce(t)
				dur := time.Since(start)
				if err != nil {
					logger.Printf("task run end: region=%s interval=%ds took=%s error=%v", t.Region, t.Interval, dur, err)
				} else {
					logger.Printf("task run end: region=%s interval=%ds took=%s", t.Region, t.Interval, dur)
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return runner{cancel: cancel, cfg: t}
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("run %s %v: %w", name, args, err)
	}
	return nil
}

func sha1Bytes(b []byte) []byte {
	h := sha1.New()
	_, _ = h.Write(b)
	return h.Sum(nil)
}
