package middleware

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/darkweak/souin/configurationtypes"
	"github.com/darkweak/souin/context"
	"github.com/darkweak/souin/helpers"
	"github.com/darkweak/souin/pkg/api"
	"github.com/darkweak/souin/pkg/api/prometheus"
	"github.com/darkweak/souin/pkg/rfc"
	"github.com/darkweak/souin/pkg/storage"
	"github.com/darkweak/souin/pkg/surrogate"
	"github.com/darkweak/souin/pkg/surrogate/providers"
	"github.com/pquerna/cachecontrol/cacheobject"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func NewHTTPCacheHandler(c configurationtypes.AbstractConfigurationInterface) *SouinBaseHandler {
	if c.GetLogger() == nil {
		var logLevel zapcore.Level
		if c.GetLogLevel() == "" {
			logLevel = zapcore.FatalLevel
		} else if err := logLevel.UnmarshalText([]byte(c.GetLogLevel())); err != nil {
			logLevel = zapcore.FatalLevel
		}
		cfg := zap.Config{
			Encoding:         "json",
			Level:            zap.NewAtomicLevelAt(logLevel),
			OutputPaths:      []string{"stderr"},
			ErrorOutputPaths: []string{"stderr"},
			EncoderConfig: zapcore.EncoderConfig{
				MessageKey: "message",

				LevelKey:    "level",
				EncodeLevel: zapcore.CapitalLevelEncoder,

				TimeKey:    "time",
				EncodeTime: zapcore.ISO8601TimeEncoder,

				CallerKey:    "caller",
				EncodeCaller: zapcore.ShortCallerEncoder,
			},
		}
		logger, _ := cfg.Build()
		c.SetLogger(logger)
	}

	storer, err := storage.NewStorage(c)
	if err != nil {
		panic(err)
	}
	c.GetLogger().Debug("Storer initialized.")
	regexpUrls := helpers.InitializeRegexp(c)
	surrogateStorage := surrogate.InitializeSurrogate(c)
	c.GetLogger().Debug("Surrogate storage initialized.")
	var excludedRegexp *regexp.Regexp = nil
	if c.GetDefaultCache().GetRegex().Exclude != "" {
		excludedRegexp = regexp.MustCompile(c.GetDefaultCache().GetRegex().Exclude)
	}

	ctx := context.GetContext()
	ctx.Init(c)

	bufPool := &sync.Pool{
		New: func() interface{} {
			return new(bytes.Buffer)
		},
	}
	defaultMatchedUrl := configurationtypes.URL{
		TTL:                 configurationtypes.Duration{Duration: c.GetDefaultCache().GetTTL()},
		Headers:             c.GetDefaultCache().GetHeaders(),
		DefaultCacheControl: c.GetDefaultCache().GetDefaultCacheControl(),
	}
	c.GetLogger().Info("Souin configuration is now loaded.")

	return &SouinBaseHandler{
		Configuration:            c,
		Storer:                   storer,
		InternalEndpointHandlers: api.GenerateHandlerMap(c, storer, surrogateStorage),
		ExcludeRegex:             excludedRegexp,
		RegexpUrls:               regexpUrls,
		DefaultMatchedUrl:        defaultMatchedUrl,
		SurrogateKeyStorer:       surrogateStorage,
		context:                  ctx,
		bufPool:                  bufPool,
	}
}

type SouinBaseHandler struct {
	Configuration            configurationtypes.AbstractConfigurationInterface
	Storer                   storage.Storer
	InternalEndpointHandlers *api.MapHandler
	ExcludeRegex             *regexp.Regexp
	RegexpUrls               regexp.Regexp
	SurrogateKeys            configurationtypes.SurrogateKeys
	SurrogateKeyStorer       providers.SurrogateInterface
	DefaultMatchedUrl        configurationtypes.URL
	context                  *context.Context
	bufPool                  *sync.Pool
}

type upsreamError struct{}

func (upsreamError) Error() string {
	return "Upstream error"
}

func isCacheableCode(code int) bool {
	switch code {
	case 200, 203, 204, 206, 300, 301, 404, 405, 410, 414, 501:
		return true
	}

	return false
}

func canBypassAuthorizationRestriction(headers http.Header, bypassed []string) bool {
	for _, header := range bypassed {
		if strings.ToLower(header) == "authorization" {
			return true
		}
	}

	return strings.Contains(strings.ToLower(headers.Get("Vary")), "authorization")
}

func (s *SouinBaseHandler) Store(
	customWriter *CustomWriter,
	rq *http.Request,
	requestCc *cacheobject.RequestCacheDirectives,
	cachedKey string,
) error {
	if !isCacheableCode(customWriter.statusCode) {
		customWriter.Headers.Set("Cache-Status", fmt.Sprintf("%s; fwd=uri-miss; key=%s; detail=UNCACHEABLE-STATUS-CODE", rq.Context().Value(context.CacheName), rfc.GetCacheKeyFromCtx(rq.Context())))

		switch customWriter.statusCode {
		case 500, 502, 503, 504:
			return new(upsreamError)
		}

		return nil
	}

	if customWriter.Header().Get("Cache-Control") == "" {
		// TODO see with @mnot if mandatory to not store the response when no Cache-Control given.
		// if s.DefaultMatchedUrl.DefaultCacheControl == "" {
		// 	customWriter.Headers.Set("Cache-Status", fmt.Sprintf("%s; fwd=uri-miss; key=%s; detail=EMPTY-RESPONSE-CACHE-CONTROL", rq.Context().Value(context.CacheName), rfc.GetCacheKeyFromCtx(rq.Context())))
		// 	return nil
		// }
		customWriter.Header().Set("Cache-Control", s.DefaultMatchedUrl.DefaultCacheControl)
	}

	responseCc, _ := cacheobject.ParseResponseCacheControl(customWriter.Header().Get("Cache-Control"))
	s.Configuration.GetLogger().Sugar().Debugf("Response cache-control %+v", responseCc)
	if responseCc == nil {
		customWriter.Headers.Set("Cache-Status", fmt.Sprintf("%s; fwd=uri-miss; key=%s; detail=INVALID-RESPONSE-CACHE-CONTROL", rq.Context().Value(context.CacheName), rfc.GetCacheKeyFromCtx(rq.Context())))
		return nil
	}
	if (responseCc.PrivatePresent || rq.Header.Get("Authorization") != "") && !canBypassAuthorizationRestriction(customWriter.Header(), rq.Context().Value(context.IgnoredHeaders).([]string)) {
		customWriter.Headers.Set("Cache-Status", fmt.Sprintf("%s; fwd=uri-miss; key=%s; detail=PRIVATE-OR-AUTHENTICATED-RESPONSE", rq.Context().Value(context.CacheName), rfc.GetCacheKeyFromCtx(rq.Context())))
		return nil
	}

	currentMatchedURL := s.DefaultMatchedUrl
	if regexpURL := s.RegexpUrls.FindString(rq.Host + rq.URL.Path); regexpURL != "" {
		u := s.Configuration.GetUrls()[regexpURL]
		if u.TTL.Duration != 0 {
			currentMatchedURL.TTL = u.TTL
		}
		if len(u.Headers) != 0 {
			currentMatchedURL.Headers = u.Headers
		}
	}

	ma := currentMatchedURL.TTL.Duration
	if responseCc.SMaxAge >= 0 {
		ma = time.Duration(responseCc.SMaxAge) * time.Second
	} else if responseCc.MaxAge >= 0 {
		ma = time.Duration(responseCc.MaxAge) * time.Second
	}
	if ma > currentMatchedURL.TTL.Duration {
		ma = currentMatchedURL.TTL.Duration
	}

	now := rq.Context().Value(context.Now).(time.Time)
	date, _ := http.ParseTime(now.Format(http.TimeFormat))
	customWriter.Headers.Set(rfc.StoredTTLHeader, ma.String())
	ma = ma - time.Since(date)

	if exp := customWriter.Header().Get("Expires"); exp != "" {
		delta, _ := time.Parse(exp, time.RFC1123)
		if sub := delta.Sub(now); sub > 0 {
			ma = sub
		}
	}

	status := fmt.Sprintf("%s; fwd=uri-miss", rq.Context().Value(context.CacheName))
	if !requestCc.NoStore && !responseCc.NoStore {
		headers := customWriter.Headers.Clone()
		for hname, shouldDelete := range responseCc.NoCache {
			if shouldDelete {
				headers.Del(hname)
			}
		}
		res := http.Response{
			StatusCode: customWriter.statusCode,
			Body:       io.NopCloser(bytes.NewBuffer(customWriter.Buf.Bytes())),
			Header:     headers,
		}

		if res.Header.Get("Date") == "" {
			res.Header.Set("Date", now.Format(http.TimeFormat))
		}
		response, err := httputil.DumpResponse(&res, true)
		if err == nil {
			variedHeaders := rfc.HeaderAllCommaSepValues(res.Header)
			cachedKey += rfc.GetVariedCacheKey(rq, variedHeaders)
			s.Configuration.GetLogger().Sugar().Debugf("Store the response %+v with duration %v", res, ma)
			if s.Storer.Set(cachedKey, response, currentMatchedURL, ma) == nil {
				s.Configuration.GetLogger().Sugar().Debugf("Store the cache key %s into the surrogate keys from the following headers %v", cachedKey, res)
				go func(rs http.Response, key string) {
					_ = s.SurrogateKeyStorer.Store(&rs, key)
				}(res, cachedKey)
				status += "; stored"
			} else {
				status += "; detail=STORAGE-INSERTION-ERROR"
			}
		}
	} else {
		status += "; detail=NO-STORE-DIRECTIVE"
	}
	customWriter.Headers.Set("Cache-Status", status+"; key="+rfc.GetCacheKeyFromCtx(rq.Context()))

	return nil
}

func (s *SouinBaseHandler) Upstream(
	customWriter *CustomWriter,
	rq *http.Request,
	next handlerFunc,
	requestCc *cacheobject.RequestCacheDirectives,
	cachedKey string,
) error {
	s.Configuration.GetLogger().Sugar().Debug("Request the upstream server")
	prometheus.Increment(prometheus.RequestCounter)
	if err := next(customWriter, rq); err != nil {
		customWriter.Header().Set("Cache-Status", fmt.Sprintf("%s; fwd=uri-miss; key=%s; detail=SERVE-HTTP-ERROR", rq.Context().Value(context.CacheName), rfc.GetCacheKeyFromCtx(rq.Context())))
		return err
	}

	if !isCacheableCode(customWriter.statusCode) {
		customWriter.Headers.Set("Cache-Status", fmt.Sprintf("%s; fwd=uri-miss; key=%s; detail=UNCACHEABLE-STATUS-CODE", rq.Context().Value(context.CacheName), rfc.GetCacheKeyFromCtx(rq.Context())))

		switch customWriter.statusCode {
		case 500, 502, 503, 504:
			return new(upsreamError)
		}

		return nil
	}

	if customWriter.Header().Get("Cache-Control") == "" {
		// TODO see with @mnot if mandatory to not store the response when no Cache-Control given.
		// if s.DefaultMatchedUrl.DefaultCacheControl == "" {
		// 	customWriter.Headers.Set("Cache-Status", fmt.Sprintf("%s; fwd=uri-miss; key=%s; detail=EMPTY-RESPONSE-CACHE-CONTROL", rq.Context().Value(context.CacheName), rfc.GetCacheKeyFromCtx(rq.Context())))
		// 	return nil
		// }
		customWriter.Header().Set("Cache-Control", s.DefaultMatchedUrl.DefaultCacheControl)
	}

	return s.Store(customWriter, rq, requestCc, cachedKey)
}

func (s *SouinBaseHandler) Revalidate(validator *rfc.Revalidator, next handlerFunc, customWriter *CustomWriter, rq *http.Request, requestCc *cacheobject.RequestCacheDirectives, cachedKey string) error {
	s.Configuration.GetLogger().Sugar().Debug("Revalidate the request with the upstream server")
	prometheus.Increment(prometheus.RequestRevalidationCounter)
	err := next(customWriter, rq)

	if err == nil {
		if validator.IfUnmodifiedSincePresent && customWriter.statusCode != http.StatusNotModified {
			customWriter.Buf.Reset()
			for h, v := range customWriter.Headers {
				if len(v) > 0 {
					customWriter.Rw.Header().Set(h, strings.Join(v, ", "))
				}
			}
			customWriter.Rw.WriteHeader(http.StatusPreconditionFailed)

			return errors.New("")
		}

		if customWriter.statusCode != http.StatusNotModified {
			err = s.Store(customWriter, rq, requestCc, cachedKey)
		}
	}

	customWriter.Header().Set(
		"Cache-Status",
		fmt.Sprintf(
			"%s; fwd=request; fwd-status=%d; key=%s; detail=REQUEST-REVALIDATION",
			rq.Context().Value(context.CacheName),
			customWriter.statusCode,
			rfc.GetCacheKeyFromCtx(rq.Context()),
		),
	)
	return err
}

func (s *SouinBaseHandler) HandleInternally(r *http.Request) (bool, http.HandlerFunc) {
	if s.InternalEndpointHandlers != nil {
		for k, handler := range *s.InternalEndpointHandlers.Handlers {
			if strings.Contains(r.RequestURI, k) {
				return true, handler
			}
		}
	}

	return false, nil
}

type handlerFunc = func(http.ResponseWriter, *http.Request) error

func (s *SouinBaseHandler) ServeHTTP(rw http.ResponseWriter, rq *http.Request, next handlerFunc) error {
	s.Configuration.GetLogger().Sugar().Debugf("Incomming request %+v", rq)
	if b, handler := s.HandleInternally(rq); b {
		handler(rw, rq)
		return nil
	}

	rq = s.context.SetBaseContext(rq)
	cacheName := rq.Context().Value(context.CacheName).(string)
	if rq.Header.Get("Upgrade") == "websocket" || (s.ExcludeRegex != nil && s.ExcludeRegex.MatchString(rq.RequestURI)) {
		rw.Header().Set("Cache-Status", cacheName+"; fwd=bypass; detail=EXCLUDED-REQUEST-URI")
		return next(rw, rq)
	}

	if !rq.Context().Value(context.SupportedMethod).(bool) {
		rw.Header().Set("Cache-Status", cacheName+"; fwd=bypass; detail=UNSUPPORTED-METHOD")

		return next(rw, rq)
	}

	requestCc, coErr := cacheobject.ParseRequestCacheControl(rq.Header.Get("Cache-Control"))

	if coErr != nil || requestCc == nil {
		rw.Header().Set("Cache-Status", cacheName+"; fwd=bypass; detail=CACHE-CONTROL-EXTRACTION-ERROR")

		return next(rw, rq)
	}

	rq = s.context.SetContext(rq)
	if rq.Context().Value(context.IsMutationRequest).(bool) {
		rw.Header().Set("Cache-Status", cacheName+"; fwd=bypass; detail=IS-MUTATION-REQUEST")

		return nil
	}
	cachedKey := rq.Context().Value(context.Key).(string)

	bufPool := s.bufPool.Get().(*bytes.Buffer)
	bufPool.Reset()
	defer s.bufPool.Put(bufPool)
	customWriter := NewCustomWriter(rq, rw, bufPool)
	s.Configuration.GetLogger().Sugar().Debugf("Request cache-control %+v", requestCc)
	if !requestCc.NoCache {
		validator := rfc.ParseRequest(rq)
		response := s.Storer.Prefix(cachedKey, rq, validator)

		if response != nil && rfc.ValidateCacheControl(response, requestCc) {
			if validator.NeedRevalidation {
				err := s.Revalidate(validator, next, customWriter, rq, requestCc, cachedKey)
				_, _ = io.Copy(customWriter.Buf, response.Body)
				_, _ = customWriter.Send()

				return err
			}
			rfc.SetCacheStatusHeader(response)
			if rfc.ValidateMaxAgeCachedResponse(requestCc, response) != nil {
				customWriter.Headers = response.Header
				customWriter.statusCode = response.StatusCode
				_, _ = io.Copy(customWriter.Buf, response.Body)
				_, err := customWriter.Send()

				return err
			}
		} else if response == nil && !requestCc.OnlyIfCached && (requestCc.MaxStaleSet || requestCc.MaxStale > -1) {
			response = s.Storer.Prefix(storage.StalePrefix+cachedKey, rq, validator)
			if nil != response && rfc.ValidateCacheControl(response, requestCc) {
				addTime, _ := time.ParseDuration(response.Header.Get(rfc.StoredTTLHeader))
				rfc.SetCacheStatusHeader(response)

				responseCc, _ := cacheobject.ParseResponseCacheControl(response.Header.Get("Cache-Control"))
				if responseCc.StaleWhileRevalidate > 0 {
					customWriter.Headers = response.Header
					customWriter.statusCode = response.StatusCode
					rfc.HitStaleCache(&response.Header)
					_, _ = io.Copy(customWriter.Buf, response.Body)
					_, err := customWriter.Send()
					customWriter = NewCustomWriter(rq, rw, bufPool)
					go func(goCw *CustomWriter, goRq *http.Request, goNext func(http.ResponseWriter, *http.Request) error, goCc *cacheobject.RequestCacheDirectives, goCk string) {
						_ = s.Upstream(goCw, goRq, goNext, goCc, goCk)
					}(customWriter, rq, next, requestCc, cachedKey)
					buf := s.bufPool.Get().(*bytes.Buffer)
					buf.Reset()
					defer s.bufPool.Put(buf)

					return err
				}

				if responseCc.MustRevalidate || responseCc.NoCachePresent || validator.NeedRevalidation {
					err := s.Revalidate(validator, next, customWriter, rq, requestCc, cachedKey)
					if err != nil {
						if responseCc.StaleIfError > -1 || requestCc.StaleIfError > 0 {
							code := fmt.Sprintf("; fwd-status=%d", customWriter.statusCode)
							customWriter.Headers = response.Header
							customWriter.statusCode = response.StatusCode
							rfc.HitStaleCache(&response.Header)
							response.Header.Set("Cache-Status", response.Header.Get("Cache-Status")+code)
							_, _ = io.Copy(customWriter.Buf, response.Body)
							_, err := customWriter.Send()

							return err
						}
						rw.WriteHeader(http.StatusGatewayTimeout)
						customWriter.Buf.Reset()
						_, err := customWriter.Send()

						return err
					}
					_, _ = io.Copy(customWriter.Buf, response.Body)
					_, _ = customWriter.Send()

					return err
				}

				// if responseCc.StaleIfError > 0 && s.Upstream(customWriter, rq, next, requestCc, cachedKey) != nil {
				// 	customWriter.Headers = response.Header
				// 	customWriter.statusCode = response.StatusCode
				// 	rfc.HitStaleCache(&response.Header)
				// 	_, _ = io.Copy(customWriter.Buf, response.Body)
				// 	_, err := customWriter.Send()
				//
				// 	return err
				// }

				if rfc.ValidateMaxAgeCachedStaleResponse(requestCc, response, int(addTime.Seconds())) != nil {
					customWriter.Headers = response.Header
					customWriter.statusCode = response.StatusCode
					rfc.HitStaleCache(&response.Header)
					_, _ = io.Copy(customWriter.Buf, response.Body)
					_, err := customWriter.Send()

					return err
				}
			}
		}
	}

	if err := s.Upstream(customWriter, rq, next, requestCc, cachedKey); err != nil {
		return err
	}

	_, _ = customWriter.Send()
	return nil
}
