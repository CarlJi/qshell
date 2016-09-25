package qshell

import (
	"bufio"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"qiniu/api.v6/auth/digest"
	"qiniu/api.v6/conf"
	"qiniu/log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

/*
{
	"dest_dir"		:	"/Users/jemy/Backup",
	"bucket"		:	"test-bucket",
	"domain"		:	"<Your bucket domain>",
	"access_key"	:	"<Your AccessKey>",
	"secret_key"	:	"<Your SecretKey>",
	"is_private"	:	false,
	"prefix"		:	"demo/",
	"suffix"		: 	".mp4",
	"referer"		: 	""
	"zone"			:	""
}
*/

const (
	MIN_DOWNLOAD_THREAD_COUNT = 1
	MAX_DOWNLOAD_THREAD_COUNT = 100
)

type DownloadConfig struct {
	DestDir   string `json:"dest_dir"`
	Bucket    string `json:"bucket"`
	Domain    string `json:"domain"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	IsPrivate bool   `json:"is_private"`
	Prefix    string `json:"prefix,omitempty"`
	Suffix    string `json:"suffix,omitempty"`
	Referer   string `json:"referer,omitemtpy"`
	Zone      string `json:"zone,omitempty"`
}

var downloadTasks chan func()
var initDownOnce sync.Once

func doDownload(tasks chan func()) {
	for {
		task := <-tasks
		task()
	}
}

func QiniuDownload(threadCount int, downloadConfigFile string) {
	timeStart := time.Now()
	cnfFp, openErr := os.Open(downloadConfigFile)
	if openErr != nil {
		log.Error("Open download config file", downloadConfigFile, "failed,", openErr)
		return
	}
	defer cnfFp.Close()
	cnfData, rErr := ioutil.ReadAll(cnfFp)
	if rErr != nil {
		log.Error("Read download config file error", rErr)
		return
	}
	downConfig := DownloadConfig{}
	cnfErr := json.Unmarshal(cnfData, &downConfig)
	if cnfErr != nil {
		log.Error("Parse download config error", cnfErr)
		return
	}

	if downConfig.Zone != "" && !IsValidZone(downConfig.Zone) {
		log.Errorf("Invalid zone setting `%s` in config file, download halt", downConfig.Zone)
		return
	}

	//set default hosts
	switch downConfig.Zone {
	case ZoneAWS:
		SetZone(ZoneAWSConfig)
	case ZoneBC:
		SetZone(ZoneBCConfig)
	case ZoneHN:
		SetZone(ZoneHNConfig)
	case ZoneNA0:
		SetZone(ZoneNA0Config)
	default:
		SetZone(ZoneNBConfig)
	}

	ioProxyHost := conf.IO_HOST

	//create local list file
	cnfJson, _ := json.Marshal(&downConfig)
	jobId := fmt.Sprintf("%x", md5.Sum(cnfJson))
	jobListName := fmt.Sprintf("%s.list.txt", jobId)
	acct := Account{
		AccessKey: downConfig.AccessKey,
		SecretKey: downConfig.SecretKey,
	}
	bLister := ListBucket{
		Account: acct,
	}
	log.Info("List bucket ...")
	listErr := bLister.List(downConfig.Bucket, downConfig.Prefix, jobListName)
	if listErr != nil {
		log.Error("List bucket error", listErr)
		return
	}
	listFp, openErr := os.Open(jobListName)
	if openErr != nil {
		log.Error("Open list file error", openErr)
		return
	}
	defer listFp.Close()
	listScanner := bufio.NewScanner(listFp)
	listScanner.Split(bufio.ScanLines)
	downWaitGroup := sync.WaitGroup{}

	totalCount := 0
	existsCount := 0

	var successCount int32 = 0
	var failCount int32 = 0

	initDownOnce.Do(func() {
		downloadTasks = make(chan func(), threadCount)
		for i := 0; i < threadCount; i++ {
			go doDownload(downloadTasks)
		}
	})

	for listScanner.Scan() {
		totalCount += 1
		line := strings.TrimSpace(listScanner.Text())
		items := strings.Split(line, "\t")
		if len(items) > 2 {
			fileKey := items[0]
			//check suffix
			if downConfig.Suffix != "" && !strings.HasSuffix(fileKey, downConfig.Suffix) {
				continue
			}
			fileSize, _ := strconv.ParseInt(items[1], 10, 64)
			//not backup yet

			if !checkLocalDuplicate(downConfig.DestDir, fileKey, fileSize) {
				downWaitGroup.Add(1)
				downloadTasks <- func() {
					defer downWaitGroup.Done()
					downErr := downloadFile(downConfig, fileKey, ioProxyHost)
					if downErr != nil {
						atomic.AddInt32(&failCount, 1)
					} else {
						atomic.AddInt32(&successCount, 1)
					}
				}
			} else {
				existsCount += 1
			}
		}
	}
	downWaitGroup.Wait()

	log.Info("-------Download Result-------")
	log.Info("Total:\t", totalCount)
	log.Info("Local:\t", existsCount)
	log.Info("Success:\t", successCount)
	log.Info("Failure:\t", failCount)
	log.Info("Duration:\t", time.Since(timeStart))
	log.Info("-----------------------------")
}

func checkLocalDuplicate(destDir string, fileKey string, fileSize int64) bool {
	dup := false
	filePath := filepath.Join(destDir, fileKey)
	fStat, statErr := os.Stat(filePath)
	if statErr == nil {
		//exist, check file size
		localFileSize := fStat.Size()
		if localFileSize == fileSize {
			dup = true
		}
	}
	return dup
}

func downloadFile(downConfig DownloadConfig, fileKey, ioProxyHost string) (err error) {
	localFilePath := filepath.Join(downConfig.DestDir, fileKey)
	ldx := strings.LastIndex(localFilePath, string(os.PathSeparator))
	if ldx != -1 {
		localFileDir := localFilePath[:ldx]
		mkdirErr := os.MkdirAll(localFileDir, 0775)
		if mkdirErr != nil {
			err = mkdirErr
			log.Error("MkdirAll failed for", localFileDir, mkdirErr.Error())
			return
		}
	}
	log.Info("Downloading", fileKey, "=>", localFilePath, "...")

	var reqHost string
	schemaIndex := strings.Index(downConfig.Domain, "://")
	if schemaIndex != -1 {
		reqHost = downConfig.Domain[schemaIndex+3:]
	} else {
		reqHost = downConfig.Domain
	}

	downUrl := fmt.Sprintf("http://%s/%s", reqHost, fileKey)
	if downConfig.IsPrivate {
		now := time.Now().Add(time.Second * 3600 * 24)
		downUrl = fmt.Sprintf("%s?e=%d", downUrl, now.Unix())
		mac := digest.Mac{downConfig.AccessKey, []byte(downConfig.SecretKey)}
		token := digest.Sign(&mac, []byte(downUrl))
		downUrl = fmt.Sprintf("%s&token=%s", downUrl, token)
	}

	//use proxy
	downUrl = strings.Replace(downUrl, fmt.Sprintf("http://%s", reqHost), ioProxyHost, -1)
	//new request
	req, reqErr := http.NewRequest("GET", downUrl, nil)
	if reqErr != nil {
		err = reqErr
		log.Error("New request", fileKey, "failed by url", downUrl, reqErr.Error())
		return
	}
	if downConfig.Referer != "" {
		req.Header.Add("Referer", downConfig.Referer)
	}
	//set request host
	req.Host = reqHost
	resp, respErr := http.DefaultClient.Do(req)
	if respErr != nil {
		err = respErr
		log.Error("Download", fileKey, "failed by url", downUrl, respErr.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		localFp, openErr := os.Create(localFilePath)
		if openErr != nil {
			err = openErr
			log.Error("Open local file", localFilePath, "failed", openErr.Error())
			return
		}
		defer localFp.Close()
		_, cpErr := io.Copy(localFp, resp.Body)
		if cpErr != nil {
			err = cpErr
			log.Error("Download", fileKey, "failed", cpErr.Error())
			return
		}
	} else {
		err = errors.New("download failed")
		log.Error("Download", fileKey, "failed by url", downUrl, resp.Status)
		return
	}
	return
}
