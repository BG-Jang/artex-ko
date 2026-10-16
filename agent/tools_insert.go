package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// assetInterceptCandidates는 삽입 대기 자산 입력 항목 하나에서 도메인/IP/URL 후보 문자열을 추출하여 자산 차단 매칭에 사용한다.
// URL의 host는 별도로 분류하여, URL만 가진 서비스/엔드포인트 자산도 도메인/IP 규칙에 걸리도록 한다.
func assetInterceptCandidates(item assetInputItem) (domains, ips, urls []string) {
	add := func(dst *[]string, s string) {
		if s = strings.TrimSpace(s); s != "" {
			*dst = append(*dst, s)
		}
	}
	add(&domains, item.Domain)
	for _, d := range item.BoundDomains {
		add(&domains, d)
	}
	add(&ips, item.IP)
	add(&ips, item.ServiceIP)
	add(&urls, item.URL)
	if item.URL != "" {
		if u, err := url.Parse(item.URL); err == nil {
			if h := u.Hostname(); h != "" {
				if net.ParseIP(h) != nil {
					add(&ips, h)
				} else {
					add(&domains, h)
				}
			}
		}
	}
	return domains, ips, urls
}

// assetInputLabel은 삽입 대기 자산 하나의 짧은 식별자를 반환한다. 차단 안내 메시지에 사용된다.
func assetInputLabel(item assetInputItem) string {
	typ := strings.TrimSpace(item.Type)
	var target string
	switch {
	case strings.TrimSpace(item.Domain) != "":
		target = strings.TrimSpace(item.Domain)
	case strings.TrimSpace(item.URL) != "":
		target = strings.TrimSpace(item.URL)
	case strings.TrimSpace(item.IP) != "":
		target = strings.TrimSpace(item.IP)
	case strings.TrimSpace(item.ServiceIP) != "":
		target = strings.TrimSpace(item.ServiceIP)
	default:
		target = "(알 수 없음)"
	}
	if typ != "" {
		return fmt.Sprintf("[%s] %s", typ, target)
	}
	return target
}

// =====================================================================
// Unified asset insertion tools
// =====================================================================

// SetAssetStore wires the asset store and company store onto this ToolSet
// so the insert_assets, add_company_scope, and list_assets tools are active.
func (t *ToolSet) SetAssetStore(as *db.AssetStore, cs *db.CompanyStore) {
	t.as = as
	t.cs = cs
}

// assetInputItem is one element of the insert_assets "assets" array.
type assetInputItem struct {
	Type string `json:"type"` // root_domain|ip|subdomain|app|service|endpoint

	// ---- root_domain / subdomain ----
	Domain      string   `json:"domain"`
	ICP         string   `json:"icp"`
	RecordType  string   `json:"record_type"`
	RecordValue []string `json:"record_value"`

	// ---- ip ----
	IP           string           `json:"ip"`
	BoundDomains []string         `json:"bound_domains"`
	OpenPorts    []db.PortService `json:"open_ports"`

	// ---- app ----
	AppName     string `json:"app_name"`
	BundleID    string `json:"bundle_id"`
	Category    string `json:"category"`
	Description string `json:"description"`
	AppICP      string `json:"app_icp"`
	CompanyID   *int64 `json:"company_id"` // explicit company link (app only; others auto-attribute via scope)

	// ---- service (http) ----
	URL           string           `json:"url"`
	Technologies  []string         `json:"technologies"`
	StatusCode    *int             `json:"status_code"`
	ContentLength *int64           `json:"content_length"`
	PageTitle     string           `json:"page_title"`
	FaviconMMH3   string           `json:"favicon_mmh3"`
	Auth          []map[string]any `json:"auth"`
	ServiceName   string           `json:"service_name"`
	ServiceIP     string           `json:"service_ip"` // optional enrichment IP

	// ---- service (other) ----
	Port  int    `json:"port"`
	Proto string `json:"proto"`

	// ---- endpoint ----
	Method string           `json:"method"`
	Params []map[string]any `json:"params"`
}

// insertAssets is the unified insert_assets agent tool.
func (t *ToolSet) insertAssets() actool.CoreTool {
	return writeTool(
		"insert_assets",
		"새로 발견한 자산을 일괄 등록한다. 한 번에 여러 유형을 섞어서 보낼 수 있다(type은 열거값 참조).\n"+
			"유형별 필수 필드: root_domain→domain; ip→ip(IPv4/IPv6 주소여야 하며 호스트명 불가); subdomain→domain; app→app_name; service(HTTP)→url; service(비HTTP)→service_name+port(ip/domain 중 하나 이상 입력); endpoint→url+method. 나머지 필드의 의미는 각 설명을 참고한다.\n"+
			"auth/technologies/params는 추가 병합(append)되며, 기존 값을 덮어쓰지 않는다.\n"+
			"반환: {results:[{index,id,type}], errors:[{index,error}]}",
		obj(map[string]any{
			// task_id는 모델에 노출하지 않는다. worker가 속한 task는 프로그램이 SetTaskID를 통해 권위 있게 지정한다(handler 참조).
			"assets": map[string]any{
				"type":        "array",
				"description": "자산 배열. 각 요소는 자산 레코드 하나에 대응한다",
				"items": obj(map[string]any{
					"type": map[string]any{
						"type":        "string",
						"enum":        []string{"root_domain", "ip", "subdomain", "app", "service", "endpoint"},
						"description": "자산 유형",
					},
					// root_domain / subdomain
					"domain":      str("루트 도메인 또는 서브도메인(root_domain/subdomain은 필수)"),
					"icp":         str("ICP 등록번호(선택)"),
					"record_type": str("DNS 레코드 유형: A/AAAA/CNAME/MX 등(subdomain은 선택)"),
					"record_value": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "DNS 레코드 값 목록(subdomain은 선택, 예: [\"1.2.3.4\",\"2.3.4.5\"])",
					},
					// ip
					"ip": str("IP 주소. 반드시 IPv4/IPv6 주소여야 하며 호스트명은 입력할 수 없다(호스트명은 type=subdomain의 domain 필드를 사용). ip 유형은 필수. service/endpoint 유형에서는 IP 연결용으로 입력할 수 있다"),
					"bound_domains": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "이 IP에 바인딩된 도메인 목록(ip 유형은 선택)",
					},
					"open_ports": map[string]any{
						"type":        "array",
						"description": "열린 포트 목록(ip 유형은 선택)",
						"items": obj(map[string]any{
							"port":    intp("포트 번호"),
							"service": str("서비스 이름, 예: http/ssh/mysql 등(선택)"),
						}, "port"),
					},
					// app
					"app_name":    str("앱 이름(app 유형은 필수)"),
					"bundle_id":   str("Bundle ID(app 유형은 선택)"),
					"category":    str("앱 분류(선택)"),
					"description": str("앱 설명(선택)"),
					"app_icp":     str("앱 ICP 등록번호(선택)"),
					"company_id":  intp("소속 기업 id(app 유형은 선택. app은 scope로 자동 귀속할 수 없으므로 명시적으로 지정해야 함. id는 add_company_scope가 반환)"),
					// service (http)
					"url":         str("전체 URL, 프로토콜과 포트 포함(HTTP 서비스는 필수. service_type은 자동으로 http로 설정)"),
					"status_code": intp("HTTP 응답 상태 코드, 예: 200/301/403/404(선택)"),
					"content_length": map[string]any{
						"type":        "integer",
						"description": "HTTP 응답 본문 바이트 수(선택)",
					},
					"page_title":   str("페이지 <title> 내용(선택)"),
					"favicon_mmh3": str("favicon MMH3 해시(선택)"),
					"technologies": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "핑거프린트/기술 스택 목록, 예: [\"Nginx\",\"Vue\",\"Bootstrap\"](선택)",
					},
					"auth": map[string]any{
						"type":        "array",
						"description": "발견된 인증 정보 목록. 각 항목은 type/username/password 등의 필드를 포함(선택, 추가되며 기존 값을 덮어쓰지 않음)",
						"items":       map[string]any{"type": "object"},
					},
					// service (other, 비HTTP)
					"service_name": str("서비스 이름, 예: ssh/mysql/redis(service가 비HTTP일 때 필수)"),
					"port":         intp("포트 번호(service가 비HTTP일 때 필수)"),
					// endpoint
					"method": str("HTTP 메서드: GET/POST/PUT/PATCH/DELETE 등(endpoint는 필수)"),
					"params": map[string]any{
						"type":        "array",
						"description": "요청 파라미터 목록. 각 항목은 location(query/body/header/path)/name/value/type을 포함(선택, 추가되며 기존 값을 덮어쓰지 않음)",
						"items":       map[string]any{"type": "object"},
					},
				}, "type"),
			},
		}, "assets"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("insert_assets 비활성화: AssetStore가 초기화되지 않음"), nil
			}
			var a struct {
				Assets []assetInputItem `json:"assets"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf("invalid input: " + err.Error()), nil
			}
			// task_id는 프로그램이 권위 있게 지정한다(worker: SetTaskID). 모델이 전달하는 값은 받지 않는다. 모델이 누락하거나 잘못 넘기면
			// 자산이 태스크에 귀속되지 않거나 엉뚱한 태스크에 귀속될 수 있기 때문이다. 태스크 컨텍스트가 없는 호출자(auto/pentest/chat)는 t.taskID=0이다.
			taskID := t.taskID

			type result struct {
				Index int    `json:"index"`
				ID    int64  `json:"id"`
				Type  string `json:"type"`
			}
			type errEntry struct {
				Index int    `json:"index"`
				Error string `json:"error"`
			}

			var results []result
			var errs []errEntry

			// 자산 게이트 규칙은 한 번에 로드한다. 읽기에 실패하면 판정을 건너뛴다(삽입은 차단하지 않음).
			// 차단 규칙 = 전역 ∪ 태스크 수준 block; 허용 규칙 = 태스크 수준 allow.
			blockRules, _ := t.as.ListAssetInterceptRules()
			var allowRules []db.AssetInterceptRule
			if t.taskID > 0 {
				if tb, ta, err := t.as.TaskInterceptRulesSplit(t.taskID); err == nil {
					blockRules = append(blockRules, tb...)
					allowRules = ta
				}
			}

			for i, item := range a.Assets {
				// 자산 게이트: 차단을 먼저 검사하고 그다음 허용을 검사한다. 거부된 자산은 삽입을 금지한다(Upsert 및 후속 부작용 생략).
				domains, ips, urls := assetInterceptCandidates(item)
				if d := db.EvaluateAssetGate(blockRules, allowRules, domains, ips, urls); !d.Allowed {
					errs = append(errs, errEntry{
						Index: i,
						Error: fmt.Sprintf("자산 %s %s, 삽입이 금지되었습니다", assetInputLabel(item), d.Reason),
					})
					continue
				}

				typ := strings.TrimSpace(item.Type)
				var id int64
				var err error

				switch typ {
				case "root_domain":
					id, err = t.as.UpsertRootDomain(db.UpsertRootDomainReq{
						Domain: item.Domain,
						ICP:    item.ICP,
						TaskID: taskID,
					})

				case "ip":
					id, err = t.as.UpsertIP(db.UpsertIPReq{
						IP:           item.IP,
						BoundDomains: item.BoundDomains,
						OpenPorts:    item.OpenPorts,
						TaskID:       taskID,
					})

				case "subdomain":
					id, err = t.as.UpsertSubdomain(db.UpsertSubdomainReq{
						Domain:      item.Domain,
						RecordType:  item.RecordType,
						RecordValue: item.RecordValue,
						ICP:         item.ICP,
						TaskID:      taskID,
					})

				case "app":
					id, err = t.as.UpsertApp(db.UpsertAppReq{
						Name:        item.AppName,
						BundleID:    item.BundleID,
						Category:    item.Category,
						Description: item.Description,
						ICP:         item.AppICP,
						CompanyID:   item.CompanyID,
						TaskID:      taskID,
					})

				case "service":
					// distinguish HTTP vs other by presence of url
					if item.URL != "" {
						// agent may send "ip" or "service_ip" for the enrichment IP; accept both
						svcIP := item.ServiceIP
						if svcIP == "" {
							svcIP = item.IP
						}
						id, err = t.as.UpsertHTTPService(db.UpsertHTTPServiceReq{
							URL:           item.URL,
							Technologies:  item.Technologies,
							StatusCode:    item.StatusCode,
							ContentLength: item.ContentLength,
							PageTitle:     item.PageTitle,
							FaviconMMH3:   item.FaviconMMH3,
							Auth:          item.Auth,
							IP:            svcIP,
							TaskID:        taskID,
						})
					} else {
						id, err = t.as.UpsertOtherService(db.UpsertOtherServiceReq{
							Domain:      item.Domain,
							IP:          item.IP,
							Port:        item.Port,
							ServiceName: item.ServiceName,
							Auth:        item.Auth,
							TaskID:      taskID,
						})
					}

				case "endpoint":
					id, err = t.as.UpsertEndpoint(db.UpsertEndpointReq{
						URL:    item.URL,
						Method: item.Method,
						Params: item.Params,
						IP:     item.ServiceIP,
						TaskID: taskID,
					})

				default:
					errs = append(errs, errEntry{Index: i, Error: "unknown type: " + typ})
					continue
				}

				if err != nil {
					errs = append(errs, errEntry{Index: i, Error: err.Error()})
					continue
				}
				results = append(results, result{Index: i, ID: id, Type: typ})
				t.writes.Assets++
				t.anchorOwner(id)
				if taskID > 0 {
					var sourceNodeID *int64
					if t.ownerNode > 0 {
						nodeID := t.ownerNode
						sourceNodeID = &nodeID
					}
					summary := "Agent가 insert_assets로 등록"
					if t.ownerNode > 0 {
						summary = fmt.Sprintf("Worker 의도 #%d이 insert_assets로 등록", t.ownerNode)
					}
					_ = t.as.SetTaskAssetSource(taskID, id, "agent", summary, sourceNodeID)
				}
				// 테스트 범위에 자동 편입(source='auto'): worker 최상위에서 명시적으로 삽입한 항목에 대해서만 그 유형에 따라
				// 보수적인 범위를 추가한다. 부작용으로 파생된 자산은 여기를 거치지 않으므로 범위가 무분별하게 넓어지지 않는다. taskID=0이면 아무 작업도 하지 않는다.
				// 커버리지 스위치와는 무관하다. task_scope는 태스크의 범위 경계(list/조회의 필터 기준)이며,
				// 커버리지 스위치는 이를 분모로 삼아 지표를 계산할지만 결정하며, 범위 자체를 누적할지는 결정하지 않는다.
				{
					svcIP := item.ServiceIP
					if svcIP == "" {
						svcIP = item.IP
					}
					_ = t.as.AddAutoScope(taskID, typ, item.Domain, item.URL, svcIP)
				}
			}

			return jsonResult(map[string]any{
				"results": results,
				"errors":  errs,
			})
		},
	)
}

// addCompanyScope writes to company_scope table and triggers asset attribution.
func (t *ToolSet) addCompanyScope() actool.CoreTool {
	return writeTool(
		"add_company_scope",
		"도메인/IP/CIDR/ICP 등록/기업 키워드를 특정 회사의 【자산 범위】에 추가한다. 도메인, 네트워크, ICP는 일치하는 자산을 자동으로 귀속시키며, 키워드는 Agent에게 범위 힌트로만 제공된다.\n"+
			"회사명은 고유하다: company가 없으면 새로 만들고, 이미 있으면 재사용한다(범위만 병합).\n"+
			"scope는 한 줄에 하나씩 입력하며, 시스템이 자동으로 인식한다: 루트 도메인 / URL / 단일 IP / CIDR 대역 / ICP 등록 / 기업 키워드.\n"+
			"반드시 reason에 귀속 근거(whois/인증서/ASN 등)를 기재한다.\n"+
			"가드레일: 베어 TLD와 지나치게 넓은 대역을 거부한다(IPv4 프리픽스는 /16-/32, IPv6 프리픽스는 /32-/128이어야 함). 잘못된 줄은 건너뛰고 errors로 반환한다.",
		obj(map[string]any{
			"company": str("회사명(없으면 새로 만들고, 있으면 재사용. 이름은 고유)"),
			"scope":   str("자산 범위, 한 줄에 하나씩: 도메인 / URL / IP / CIDR / ICP 등록 / 기업 키워드"),
			"reason":  str("귀속 근거(증거/출처), 반드시 입력"),
			"logo":    str("회사 아이콘 URL(선택; 새 회사를 만들 때만 적용)"),
		}, "company", "scope"),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.cs == nil {
				return actool.Errorf("add_company_scope 비활성화: CompanyStore가 초기화되지 않음"), nil
			}
			var a struct {
				Company string `json:"company"`
				Scope   string `json:"scope"`
				Reason  string `json:"reason"`
				Logo    string `json:"logo"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return actool.Errorf(err.Error()), nil
			}
			if strings.TrimSpace(a.Company) == "" {
				return actool.Errorf("company는 비워 둘 수 없습니다"), nil
			}
			companyID, _, err := t.cs.UpsertCompany(a.Company, a.Logo)
			if err != nil {
				return actool.Errorf("회사 생성/조회 실패: " + err.Error()), nil
			}
			lines := splitLines(a.Scope)
			added, skipped, invalid, errMsgs := t.cs.AddScope(companyID, lines, a.Reason)
			out := map[string]any{
				"company_id": companyID,
				"added":      added,
				"skipped":    skipped,
				"invalid":    invalid,
			}
			if len(errMsgs) > 0 {
				out["errors"] = errMsgs
			}
			return jsonResult(out)
		},
	)
}

// addTaskScope lets the plan agent add test scope to THE CURRENT TASK — the coverage
// denominator and the task's authorization edge. Worker discoveries are auto-scoped
// (precise host) by insertAssets; this tool is for DELIBERATELY WIDENING: pull a whole
// root domain or whole company into scope, or add a specific subdomain / ip.
func (t *ToolSet) addTaskScope() actool.CoreTool {
	return writeTool(
		"add_task_scope",
		"테스트 범위를 【이 태스크】에 추가한다. 이는 이 태스크의 인가 경계이자 자산 테스트 커버리지의 분모이다.\n"+
			"kind 지원 값: company(회사 명의 전체 자산) / root_domain(전체 루트 도메인, 모든 서브도메인 포함) / subdomain(단일 정확 서브도메인) / ip / cidr / icp / keyword.\n"+
			"참고: worker가 하나씩 마주치는 호스트는 시스템이 범위(정확한 서브도메인)에 【자동】 추가한다. 이 도구는 【능동적 확장】에 사용한다. 즉 전체 루트 도메인/전체 회사를 포함하거나, 특정 서브도메인/IP를 보충 지정한다.\n"+
			"value: company는 회사명 또는 id를 전달(회사가 이미 존재해야 함); root_domain/subdomain은 도메인을 전달; ip/cidr은 IP 또는 대역을 전달; icp/keyword는 등록번호 또는 기업 키워드를 전달.\n"+
			"반드시 reason에 근거를 기재한다(감사 가능하도록). 여러 항목은 entries 배열을 사용한다.",
		obj(map[string]any{
			"entries": map[string]any{"type": "array", "description": "일괄: [{kind, value}]. kind∈company/root_domain/subdomain/ip/cidr/icp/keyword.", "items": map[string]any{"type": "object"}},
			"kind":    str("[단건] company / root_domain / subdomain / ip / cidr / icp / keyword"),
			"value":   str("[단건] 회사명 또는 id / 도메인 / IP / CIDR / ICP / 키워드"),
			"reason":  str("추가 근거(감사용), 반드시 입력"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("add_task_scope 비활성화: AssetStore가 초기화되지 않음"), nil
			}
			if t.taskID <= 0 {
				return actool.Errorf("add_task_scope에는 태스크 컨텍스트가 필요합니다(현재 task 없음)"), nil
			}
			type scopeEntry struct {
				Kind  string `json:"kind"`
				Value string `json:"value"`
			}
			var a struct {
				Entries    []scopeEntry `json:"entries"`
				scopeEntry              // 단건 모드
				Reason     string       `json:"reason"`
			}
			_ = json.Unmarshal(in, &a)
			items := a.Entries
			if len(items) == 0 {
				items = []scopeEntry{a.scopeEntry}
			}
			var added []map[string]any
			errs := map[string]string{}
			for i, e := range items {
				ts, err := t.as.AddAgentScope(t.taskID, strings.TrimSpace(e.Kind), e.Value, a.Reason, "agent")
				if err != nil {
					errs[strconv.Itoa(i)] = err.Error()
					continue
				}
				added = append(added, map[string]any{"kind": ts.Kind, "domain": ts.Domain, "net": ts.Net, "value": ts.Value, "company_id": ts.CompanyID})
			}
			out := map[string]any{"added": added}
			if len(errs) > 0 {
				out["errors"] = errs
			}
			return jsonResult(out)
		},
	)
}

// listUntestedAssets lets the plan agent pull the current + directly inherited
// scope's not-yet-tested assets on demand (filter by type, paginated).
func (t *ToolSet) listUntestedAssets() actool.CoreTool {
	return readTool(
		"list_untested_assets",
		"【이 태스크 및 직접 연관된 태스크】 범위 내에서 아직 사실 앵커로 커버되지 않은 자산을 조회한다(연관 범위는 읽기 전용이며, 보완 테스트 여부는 직접 판단하며 대신 결정하지 않는다).\n"+
			"선택적으로 자산 유형별 필터링: root_domain/subdomain/service/app/endpoint/ip.\n"+
			"페이지네이션: page는 1부터 시작, page_size 기본값 10. 반환 {assets:[{id,type,label}], total, page, page_size}. 태스크 컨텍스트에서만 사용 가능.",
		obj(map[string]any{
			"type":      str("자산 유형 필터(선택): root_domain/subdomain/service/app/endpoint/ip"),
			"page":      intp("페이지 번호, 1부터 시작(기본값 1)"),
			"page_size": intp("페이지당 개수(기본값 10)"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("list_untested_assets 비활성화: AssetStore가 초기화되지 않음"), nil
			}
			if t.taskID <= 0 || t.ts == nil {
				return actool.Errorf("list_untested_assets에는 태스크 컨텍스트가 필요합니다"), nil
			}
			var a struct {
				Type     string `json:"type"`
				Page     int    `json:"page"`
				PageSize int    `json:"page_size"`
			}
			_ = json.Unmarshal(in, &a)
			if a.Page <= 0 {
				a.Page = 1
			}
			if a.PageSize <= 0 {
				a.PageSize = 10
			}
			offset := (a.Page - 1) * a.PageSize
			assets, total, err := t.as.ListUntestedAssetsWithSources(t.taskID, strings.TrimSpace(a.Type), a.PageSize, offset)
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(map[string]any{
				"assets": assets, "total": total, "page": a.Page, "page_size": a.PageSize,
			})
		},
	)
}

// listAssets lets an agent query the asset table.
func (t *ToolSet) listAssets() actool.CoreTool {
	return readTool(
		"list_assets",
		"자산 DB 조회: DSL 표현식 검색 또는 id/ids로 직접 조회. 페이지네이션을 지원한다. 【이 태스크 및 직접 연관된 태스크】 테스트 범위 내의 자산만 반환한다.\n"+
			"DSL: field=value 부분 일치(ILIKE) | field==value 정확 일치 | field!=value 제외 | 숫자 필드는 > >= < <= 지원 | 단독 단어=전문 부분 일치; AND/OR 조합(AND 우선순위가 더 높음), 괄호로 그룹화 가능. 자산 유형은 별도의 type 파라미터를 사용하며 DSL에 쓰지 않는다.\n"+
			"id/ids를 전달하지 않으면 dsl은 반드시 비어 있지 않아야 한다(조건 없는 전체 조회는 허용하지 않음).\n"+
			"사용 가능한 필드: domain(루트/서브/서비스 도메인), root_domain, ip, url, page_title, icp, service_name, app_name, method(예: GET/POST), service_type(http|other), record_type(예: A/CNAME), technology(배열, =부분 일치 ==정확 일치), port/status_code/company_id(정수).\n"+
			"예시: status_code>=400 AND technology=shiro ; (port==80 OR port==443) AND technology=nginx",
		obj(map[string]any{
			"dsl":    str(`DSL 조회 표현식(문법/필드는 도구 설명 참조). id/ids를 전달하지 않으면 반드시 비어 있지 않아야 함.`),
			"type":   str("자산 유형 필터: root_domain|ip|subdomain|app|service|endpoint (독립 필드, dsl과 함께 사용 가능; type만으로는 조회가 불충분하므로 dsl이 여전히 필요)"),
			"id":     intp("단일 자산 id로 직접 조회(선택, dsl/type과 상호 배타)"),
			"ids":    map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "여러 자산 id로 직접 조회(선택, dsl/type과 상호 배타)"},
			"limit":  intp("반환 상한, 기본값 10(선택)"),
			"offset": intp("페이지네이션 오프셋, 기본값 0(선택)"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.as == nil {
				return actool.Errorf("list_assets 비활성화: AssetStore가 초기화되지 않음"), nil
			}
			var a struct {
				DSL    string  `json:"dsl"`
				Type   string  `json:"type"`
				ID     int64   `json:"id"`
				IDs    []int64 `json:"ids"`
				Limit  int     `json:"limit"`
				Offset int     `json:"offset"`
			}
			_ = json.Unmarshal(in, &a)
			if a.Limit <= 0 {
				a.Limit = 10
			}

			var assets []*db.Asset
			var err error
			switch {
			case a.ID > 0:
				assets, err = t.as.GetByIDsInScope(t.taskID, []int64{a.ID})
			case len(a.IDs) > 0:
				assets, err = t.as.GetByIDsInScope(t.taskID, a.IDs)
			case a.DSL != "":
				assets, err = t.as.QueryDSLInScope(a.DSL, a.Type, t.taskID, a.Limit, a.Offset)
			default:
				return actool.Errorf("id/ids를 전달하지 않으면 dsl은 비워 둘 수 없습니다: 조건 없이 전체 자산을 조회할 수 없으니 조회 조건을 입력하세요"), nil
			}
			if err != nil {
				return actool.Errorf("DSL 오류: " + err.Error()), nil
			}
			return jsonResult(map[string]any{
				"count":  len(assets),
				"assets": assets,
			})
		},
	)
}

// listCompanies lets an agent enumerate companies (기업) with their scope + asset count.
func (t *ToolSet) listCompanies() actool.CoreTool {
	return readTool(
		"list_companies",
		"자산 DB의 【기업/회사】와 각 자산 범위(scope), 귀속된 자산 수를 나열한다. 어떤 회사가 있는지 확인하고 "+
			"company_id를 얻는 데 사용한다(insert_assets에서 app을 연결하거나 list_assets에서 company_id로 필터링할 때 사용). "+
			"선택 항목 search로 회사명 부분 일치 필터링(대소문자 무시). 비워 두면 전체를 반환한다.",
		obj(map[string]any{
			"search": str("회사명 부분 일치 필터(선택, 대소문자 무시); 비워 두면 전체 반환"),
		}),
		func(_ context.Context, in json.RawMessage) (actool.Result, error) {
			if t.cs == nil {
				return actool.Errorf("list_companies 비활성화: CompanyStore가 초기화되지 않음"), nil
			}
			var a struct {
				Search string `json:"search"`
			}
			_ = json.Unmarshal(in, &a)
			cos, err := t.cs.ListCompanies()
			if err != nil {
				return actool.Errorf("회사 조회 실패: " + err.Error()), nil
			}
			q := strings.ToLower(strings.TrimSpace(a.Search))
			type companyOut struct {
				ID         int64    `json:"id"`
				Name       string   `json:"name"`
				AssetCount int      `json:"asset_count"`
				Scope      []string `json:"scope"`
			}
			out := make([]companyOut, 0, len(cos))
			for _, c := range cos {
				if q != "" && !strings.Contains(strings.ToLower(c.Name), q) {
					continue
				}
				scope := make([]string, 0, len(c.Scope))
				for _, r := range c.Scope {
					scope = append(scope, r.Raw)
				}
				out = append(out, companyOut{ID: c.ID, Name: c.Name, AssetCount: c.AssetCount, Scope: scope})
			}
			return jsonResult(map[string]any{"count": len(out), "companies": out})
		},
	)
}

// splitLines splits a multi-line string into non-empty trimmed lines.
func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// WorkerTools returns the tool set for a work agent.
func (t *ToolSet) WorkerTools() []actool.CoreTool {
	return []actool.CoreTool{
		// list_findings 유지: 취약점을 보고하기 전에 이 태스크에서 이미 확인된 취약점을 조회하여 같은 취약점을 중복 보고하지 않도록 한다.
		t.listFindings(),
		t.addFinding(), t.recordFact(),
		// asset management (handlers guard nil store internally)。
		// add_company_scope는 worker에게 주지 않는다: 기업 자산 범위 정의는 계획/메인/Auto의 책임이며, worker는 탐색만 수행한다.
		t.insertAssets(), t.listAssets(),
		// 작업 간 회고: worker도 다른 work의 관찰 결과를 재사용하여 중복 작업을 피할 수 있다.
		// search_all_worker_traces: intent_id를 미리 알 필요 없이 키워드로 전체 단계에서 일치 항목을 찾는다;
		// get_worker_trace: 특정 work를 지정한 후 단계 나열/현 위치 검색/전체 내용 조회.
		t.searchAllWorkerTraces(), t.getWorkerTrace(),
		// node_detail: worker가 intent_id/노드 id를 얻은 후 해당 노드의 전체 상세를 조회할 수 있다(위의 회고와 함께 사용).
		t.nodeDetail(),
		// 아래 도구는 여전히 worker에게 【주지 않으며】 planner/main에만 남겨 둔다(컨텍스트 읽기와 작업 간 복기는 계획 책임이며,
		// worker는 단일 의도의 실행과 결과 기록만 수행한다): list_facts / list_companies / list_worker_traces.
	}
}

// MainAgentTools returns the human-interface tool set.
func (t *ToolSet) MainAgentTools() []actool.CoreTool {
	return []actool.CoreTool{
		t.graphOverview(), t.listFindings(), t.listFacts(), t.nodeDetail(),
		t.expandDigest(), // cold-digest §6.1
		t.getWorkerOutput(), t.getWorkerTrace(), t.searchAllWorkerTraces(), t.addHint(), t.addIntent(),
		// steer_work: 사람이 실행 중인 특정 의도(work)에 교정 지시를 실시간으로 주입할 수 있다(중단하지 않으며 진행 상황을 잃지 않음).
		t.steerWorkTool(),
		// set_goals: 사람이 실행 중에 이 태스크에 새로운 최종 목표를 추가할 수 있다(계획자가 이에 따라 달성 여부를 재판단한다).
		t.setGoals(),
		// set_constraints: 사람이 실행 중에 이 태스크의 작업 제약(allow/deny)을 추가/수정할 수 있다. 제약은 planner/worker의 탐색 경계를 정한다.
		t.setConstraints(),
		// 자산 관리 (handler가 내부적으로 nil store를 가드한다)
		t.insertAssets(), t.addCompanyScope(), t.listAssets(),
		t.addFinding(), t.recordFact(),
		t.addTaskScope(),
		// list_untested_assets: 이 태스크 범위 내의 미테스트 자산을 필요할 때 조회(유형+페이지네이션)하여 보완 테스트 여부를 스스로 결정한다.
		t.listUntestedAssets(),
	}
}

// AllDomainTools returns the union of all domain tools across all agent types,
// deduped by name (mainagent order wins). Used by the server to build a registry
// for injecting domain tools into agents (Auto, custom) that don't own a per-task
// ToolSet. The caller provides real stores; tools are callable at taskID=0 scope.
func (t *ToolSet) AllDomainTools() []actool.CoreTool {
	seen := map[string]bool{}
	var out []actool.CoreTool
	all := append(append(t.MainAgentTools(), t.PlannerTools()...), t.WorkerTools()...)
	for _, tool := range all {
		if !seen[tool.Name()] {
			seen[tool.Name()] = true
			out = append(out, tool)
		}
	}
	return out
}
