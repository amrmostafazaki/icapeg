package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"icapeg/config"
	"icapeg/dtos"
	"icapeg/service"
	"icapeg/utils"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/egirna/icap"
)

// ToICAPEGResp is the ICAP Response Mode Handler:
func ToICAPEGResp(w icap.ResponseWriter, req *icap.Request) {
	h := w.Header()
	h.Set("ISTag", utils.ISTag)
	h.Set("Service", "Egirna ICAP-EG")

	appCfg := config.App()

	log.Printf("Request received---> METHOD:%s URL:%s\n", req.Method, req.RawURL)

	switch req.Method {
	case utils.ICAPModeOptions:
		h.Set("Methods", utils.ICAPModeResp)
		h.Set("Allow", "204")
		h.Set("Preview", appCfg.PreviewBytes)
		h.Set("Transfer-Preview", utils.Any)
		w.WriteHeader(http.StatusOK, nil, false)
	case utils.ICAPModeResp:

		scannerName := strings.ToLower(appCfg.RespScannerVendor) // the name of the scanner vendor

		if scannerName == "" { // if no scanner name provided, then bypass everything
			log.Println("No respmod scanner provided...bypassing everything")
			w.WriteHeader(http.StatusNoContent, nil, false)
			return
		}

		if val, exist := req.Header["Allow"]; !exist || (len(val) > 0 && val[0] != "204") { // following RFC3507, if the request has Allow: 204 header, it is to be checked and if it doesn't exists, return the request as it is to the ICAP client, https://tools.ietf.org/html/rfc3507#section-4.6
			log.Println("Processing not required for this request")
			w.WriteHeader(http.StatusNoContent, nil, false)
			return
		}

		// getting the content type to determine if the response is for a file, and if so, if its allowed to be processed
		// according to the configuration

		ct := utils.GetMimeExtension(req.Preview)

		processExts := appCfg.ProcessExtensions
		bypassExts := appCfg.BypassExtensions

		if !(utils.InStringSlice(utils.Any, processExts) && !utils.InStringSlice(ct, bypassExts)) { // if there is no star in process and the  provided extension is in bypass
			if !utils.InStringSlice(ct, processExts) { // and its not in processable either, then don't process it
				log.Println("Processing not required for file type-", ct)
				log.Println("Reason: Doesn't belong to processable extensions")
				w.WriteHeader(http.StatusNoContent, nil, false)
				return
			}
		}

		if (utils.InStringSlice(utils.Any, bypassExts) && !utils.InStringSlice(ct, processExts)) ||
			utils.InStringSlice(ct, bypassExts) { // if there is start in bypass and there provided extension is not in process or it is in bypass, the don't process it
			log.Println("Processing not required for file type-", ct)
			log.Println("Reason: Doesn't belong to unprocessable extensions")
			w.WriteHeader(http.StatusNoContent, nil, false)
			return
		}

		// copying the file to a buffer for scanner vendor processing as the file is allowed according the co figuration

		buf := &bytes.Buffer{}

		if _, err := io.Copy(buf, req.Response.Body); err != nil {
			log.Println("Failed to copy the response body to buffer: ", err.Error())
			w.WriteHeader(http.StatusNoContent, nil, false)
			return
		}

		if buf.Len() > appCfg.MaxFileSize {
			log.Println("File size too large")
			w.WriteHeader(http.StatusNoContent, nil, false)
			return
		}

		// preparing the file meta informations
		filename := utils.GetFileName(req.Request)
		fileExt := utils.GetFileExtension(req.Request)
		fmi := dtos.FileMetaInfo{
			FileName: filename,
			FileType: fileExt,
			FileSize: float64(buf.Len()),
		}

		localService := service.IsServiceLocal(scannerName)

		if localService { // if the scanner is installed locally
			lsvc := service.GetLocalService(scannerName)

			if lsvc == nil {
				log.Println("No such scanner vendors:", scannerName)
				w.WriteHeader(utils.IfPropagateError(http.StatusBadRequest, http.StatusNoContent), nil, false)
				return
			}

			if !lsvc.RespSupported() {
				log.Printf("The vendor %s does not support respmod of icap\n", scannerName)
				w.WriteHeader(utils.IfPropagateError(http.StatusBadRequest, http.StatusNoContent), nil, false)
				return
			}

			sampleInfo, err := lsvc.ScanFileStream(buf, fmi)
			if err != nil {
				log.Println("Couldn't fetch sample information for local service: ", err.Error())
				w.WriteHeader(utils.IfPropagateError(http.StatusFailedDependency, http.StatusNoContent), nil, false)
				return
			}

			if appCfg.Debug {
				spew.Dump("result", sampleInfo)
			}

			if !utils.InStringSlice(sampleInfo.SampleSeverity, lsvc.GetOkFileStatus()) { // checking is the sample severity is amongst the allowable file status
				log.Printf("The file:%s is %s\n", filename, sampleInfo.SampleSeverity)
				htmlBuf, newResp := utils.GetTemplateBufferAndResponse(utils.BadFileTemplate, &dtos.TemplateData{
					FileName:     sampleInfo.FileName,
					FileType:     sampleInfo.SampleType,
					FileSizeStr:  sampleInfo.FileSizeStr,
					RequestedURL: utils.BreakHTTPURL(req.Request.RequestURI),
					Severity:     sampleInfo.SampleSeverity,
					Score:        sampleInfo.VTIScore,
					ResultsBy:    scannerName,
				})
				w.WriteHeader(http.StatusOK, newResp, true)
				w.Write(htmlBuf.Bytes())
				return
			}

		}

		if !localService { // if the scanner is an external service requiring API calls.
			// making necessary service api calls

			svc := service.GetService(scannerName)

			if svc == nil {
				log.Println("No such scanner vendors:", scannerName)
				w.WriteHeader(utils.IfPropagateError(http.StatusBadRequest, http.StatusNoContent), nil, false)
				return
			}

			if svc == nil {
				log.Println("No such scanner vendors:", scannerName)
				w.WriteHeader(utils.IfPropagateError(http.StatusBadRequest, http.StatusNoContent), nil, false)
				return
			}

			if !svc.RespSupported() {
				log.Printf("The vendor %s does not support respmod of icap\n", scannerName)
				w.WriteHeader(utils.IfPropagateError(http.StatusBadRequest, http.StatusNoContent), nil, false)
				return
			}

			// The submit file api call is commented out for safety for now
			submitResp, err := svc.SubmitFile(buf, filename) // submitting the file for analysing
			if err != nil {
				log.Printf("Failed to submit file to %s: %s\n", scannerName, err.Error())
				w.WriteHeader(utils.IfPropagateError(http.StatusFailedDependency, http.StatusNoContent), nil, false)
				return
			}

			if !submitResp.SubmissionExists {
				log.Println("No submissions for the file")
				w.WriteHeader(http.StatusNoContent, nil, false)
				return
			}

			if appCfg.Debug {
				spew.Dump("submit response", submitResp)
			}

			submissionFinished := false
			statusCheckFinishTime := time.Now().Add(svc.GetStatusCheckTimeout()) // the time after which, the system is to stop checking for submission finish
			var sampleInfo *dtos.SampleInfo
			sampleID := submitResp.SubmissionSampleID //"4715575"

			for !submissionFinished && time.Now().Before(statusCheckFinishTime) { // while the time dedicated for status checking has no expired
				submissionID := submitResp.SubmissionID //"5651578"

				switch svc.StatusEndpointExists() { // this blocks acts depending of the fact that the scanner has a seperate endpoint for checking file scan status or not, somne of them has and some don't
				case true:
					submissionStatus, err := svc.GetSubmissionStatus(submissionID) // getting the file submission status by the submission id received by submitting the file
					if err != nil {
						log.Printf("Failed to get submission status from %s: %s\n", scannerName, err.Error())
						w.WriteHeader(utils.IfPropagateError(http.StatusFailedDependency, http.StatusNoContent), nil, false)
						return
					}

					if appCfg.Debug {
						spew.Dump("submission status resp", submissionStatus)
					}
					submissionFinished = submissionStatus.SubmissionFinished
				case false: // if it doesn;t the file report result will contain the information
					var err error
					sampleInfo, err = svc.GetSampleFileInfo(sampleID, fmi)
					if err != nil {
						log.Println("Couldn't fetch sample information during status check: ", err.Error())
						w.WriteHeader(utils.IfPropagateError(http.StatusFailedDependency, http.StatusNoContent), nil, false)
						return
					}
					submissionFinished = sampleInfo.SubmissionFinished
				default:
					log.Println("Put the status_endpoint_exists field in the config file under the scanner vendor")
				}

				if !submissionFinished { // if the submission is not finished, wait for a certain time and then call again
					time.Sleep(svc.GetStatusCheckInterval())
				}
			}

			if !submissionFinished {
				log.Println("File submission is taking too long to finish")
				w.WriteHeader(http.StatusNoContent, nil, false)
				return
			}

			if sampleInfo == nil {
				var err error
				sampleInfo, err = svc.GetSampleFileInfo(sampleID, fmi) // getting the results after scanner is done analysing the file
				if err != nil {
					log.Println("Couldn't fetch sample information after submission finish: ", err.Error())
					w.WriteHeader(utils.IfPropagateError(http.StatusFailedDependency, http.StatusNoContent), nil, false)
					return
				}
			}

			if appCfg.Debug {
				spew.Dump("result", sampleInfo)
			}

			if !utils.InStringSlice(sampleInfo.SampleSeverity, svc.GetOkFileStatus()) { // checking is the sample severity is amongst the allowable file status
				log.Printf("The file:%s is %s\n", filename, sampleInfo.SampleSeverity)
				htmlBuf, newResp := utils.GetTemplateBufferAndResponse(utils.BadFileTemplate, &dtos.TemplateData{
					FileName:     sampleInfo.FileName,
					FileType:     sampleInfo.SampleType,
					FileSizeStr:  sampleInfo.FileSizeStr,
					RequestedURL: utils.BreakHTTPURL(req.Request.RequestURI),
					Severity:     sampleInfo.SampleSeverity,
					Score:        sampleInfo.VTIScore,
					ResultsBy:    scannerName,
				})
				w.WriteHeader(http.StatusOK, newResp, true)
				w.Write(htmlBuf.Bytes())

				return
			}
		}

		log.Printf("The file %s is good to go\n", filename)
		w.WriteHeader(http.StatusNoContent, nil, false) // all ok, show the contents as it is

	case "ERRDUMMY":
		w.WriteHeader(http.StatusBadRequest, nil, false)
		fmt.Println("Malformed request")
	default:
		w.WriteHeader(http.StatusMethodNotAllowed, nil, false)
		log.Printf("Invalid request method %s- respmod\n", req.Method)
	}
}

// ToICAPEGReq is the ICAP request Mode Handler:
func ToICAPEGReq(w icap.ResponseWriter, req *icap.Request) {
	h := w.Header()
	h.Set("ISTag", utils.ISTag)
	h.Set("Service", "Egirna ICAP-EG")

	appCfg := config.App()

	log.Printf("Request received---> METHOD:%s URL:%s\n", req.Method, req.RawURL)

	switch req.Method {
	case utils.ICAPModeOptions:
		h.Set("Methods", utils.ICAPModeReq)
		h.Set("Allow", "204")
		h.Set("Preview", "0")
		h.Set("Transfer-Preview", utils.Any)
		w.WriteHeader(http.StatusOK, nil, false)
	case utils.ICAPModeReq:

		scannerName := strings.ToLower(appCfg.ReqScannerVendor)

		if scannerName == "" {
			log.Println("No reqmod scanner provided...bypassing everything")
			w.WriteHeader(http.StatusNoContent, nil, false)
			return
		}

		if val, exist := req.Header["Allow"]; !exist || (len(val) > 0 && val[0] != "204") { // following RFC3507, if the request has Allow: 204 header, it is to be checked and if it doesn't exists, return the request as it is to the ICAP client, https://tools.ietf.org/html/rfc3507#section-4.6
			log.Println("Processing not required for this request")
			w.WriteHeader(http.StatusNoContent, nil, false)
			return
		}

		ext := utils.GetFileExtension(req.Request)

		if ext == "" {
			ext = "html"
		}

		processExts := appCfg.ProcessExtensions
		bypassExts := appCfg.BypassExtensions

		if !(utils.InStringSlice(utils.Any, processExts) && !utils.InStringSlice(ext, bypassExts)) { // if there is no star in process and the  provided extension is in bypass
			if !utils.InStringSlice(ext, processExts) { // and its not in processable either, then don't process it
				log.Println("Processing not required for file type-", ext)
				log.Println("Reason: Doesn't belong to processable extensions")
				w.WriteHeader(http.StatusNoContent, nil, false)
				return
			}
		}

		if (utils.InStringSlice(utils.Any, bypassExts) && !utils.InStringSlice(ext, processExts)) ||
			utils.InStringSlice(ext, bypassExts) { // if there is start in bypass and there provided extension is not in process or it is in bypass, the don't process it
			log.Println("Processing not required for file type-", ext)
			log.Println("Reason: Doesn't belong to unprocessable extensions")
			w.WriteHeader(http.StatusNoContent, nil, false)
			return
		}

		fileURL := req.Request.RequestURI

		// preparing the file meta informations
		filename := utils.GetFileName(req.Request)
		fileExt := utils.GetFileExtension(req.Request)
		fmi := dtos.FileMetaInfo{
			FileName: filename,
			FileType: fileExt,
		}

		svc := service.GetService(scannerName)

		if svc == nil {
			log.Println("No such scanner vendors:", scannerName)
			w.WriteHeader(utils.IfPropagateError(http.StatusBadRequest, http.StatusNoContent), nil, false)
			return
		}

		if !svc.ReqSupported() {
			log.Printf("The vendor %s does not support reqmod of icap\n", scannerName)
			w.WriteHeader(utils.IfPropagateError(http.StatusBadRequest, http.StatusNoContent), nil, false)
			return
		}

		// The submit file api call is commented out for safety for now
		submitResp, err := svc.SubmitURL(fileURL, filename) // submitting the file for analysing
		if err != nil {
			log.Printf("Failed to submit url to %s: %s\n", scannerName, err.Error())
			w.WriteHeader(utils.IfPropagateError(http.StatusFailedDependency, http.StatusNoContent), nil, false)
			return
		}

		if !submitResp.SubmissionExists {
			log.Println("No submissions for the file")
			w.WriteHeader(http.StatusNoContent, nil, false)
			return
		}

		if appCfg.Debug {
			spew.Dump("submit response", submitResp)
		}

		submissionFinished := false
		statusCheckFinishTime := time.Now().Add(svc.GetStatusCheckTimeout()) // the time after which, the system is to stop checking for submission finish
		var sampleInfo *dtos.SampleInfo
		sampleID := submitResp.SubmissionSampleID //"4715575"

		for !submissionFinished && time.Now().Before(statusCheckFinishTime) { // while the time dedicated for status checking has no expired
			submissionID := submitResp.SubmissionID //"5651578"

			switch svc.StatusEndpointExists() { // this blocks acts depending of the fact that the scanner has a seperate endpoint for checking file scan status or not, somne of them has and some don't
			case true:
				submissionStatus, err := svc.GetSubmissionStatus(submissionID) // getting the file submission status by the submission id received by submitting the file
				if err != nil {
					log.Printf("Failed to get submission status from %s: %s\n", scannerName, err.Error())
					w.WriteHeader(utils.IfPropagateError(http.StatusFailedDependency, http.StatusNoContent), nil, false)
					return
				}

				if appCfg.Debug {
					spew.Dump("submission status resp", submissionStatus)
				}
				submissionFinished = submissionStatus.SubmissionFinished
			case false: // if it doesn;t the file report result will contain the information
				var err error
				sampleInfo, err = svc.GetSampleURLInfo(sampleID, fmi)
				if err != nil {
					log.Println("Couldn't fetch sample information during status check: ", err.Error())
					w.WriteHeader(utils.IfPropagateError(http.StatusFailedDependency, http.StatusNoContent), nil, false)
					return
				}
				submissionFinished = sampleInfo.SubmissionFinished
			default:
				log.Println("Put the status_endpoint_exists field in the config file under the scanner vendor")
			}

			if !submissionFinished { // if the submission is not finished, wait for a certain time and then call again
				time.Sleep(svc.GetStatusCheckInterval())
			}
		}

		if !submissionFinished {
			log.Println("File submission is taking too long to finish")
			w.WriteHeader(http.StatusNoContent, nil, false)
			return
		}

		if sampleInfo == nil {
			var err error
			sampleInfo, err = svc.GetSampleURLInfo(sampleID, fmi) // getting the results after scanner is done analysing the file
			if err != nil {
				log.Println("Couldn't fetch sample information after submission finish: ", err.Error())
				w.WriteHeader(utils.IfPropagateError(http.StatusFailedDependency, http.StatusNoContent), nil, false)
				return
			}
		}

		if appCfg.Debug {
			spew.Dump("result", sampleInfo)
		}

		if !utils.InStringSlice(sampleInfo.SampleSeverity, svc.GetOkFileStatus()) { // checking is the sample severity is amongst the allowable file status
			log.Printf("The url:%s is %s\n", filename, sampleInfo.SampleSeverity)
			data := &dtos.TemplateData{
				FileName:     sampleInfo.FileName,
				FileType:     sampleInfo.SampleType,
				FileSizeStr:  sampleInfo.FileSizeStr,
				RequestedURL: utils.BreakHTTPURL(req.Request.RequestURI),
				Severity:     sampleInfo.SampleSeverity,
				Score:        sampleInfo.VTIScore,
				ResultsBy:    scannerName,
			}

			dataByte, err := json.Marshal(data)

			if err != nil {
				log.Println("Failed to marshal template data: ", err.Error())
				w.WriteHeader(utils.IfPropagateError(http.StatusInternalServerError, http.StatusNoContent), nil, false)
				return
			}

			req.Request.Body = ioutil.NopCloser(bytes.NewReader(dataByte))

			icap.ServeLocally(w, req)

			return
		}

		log.Printf("The url %s is good to go\n", fileURL)
		w.WriteHeader(http.StatusNoContent, nil, false)

	case "ERRDUMMY":
		w.WriteHeader(http.StatusBadRequest, nil, false)
		fmt.Println("Malformed request")
	default:
		w.WriteHeader(http.StatusMethodNotAllowed, nil, false)
		log.Printf("Invalid request method %s- reqmod\n", req.Method)

	}
}
