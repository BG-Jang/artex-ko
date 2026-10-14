-- ARTEX PostgreSQL 스키마 (단일 데이터 소스)
-- 멱등성: 반복 실행해도 안전하다 (IF NOT EXISTS / OR REPLACE / DROP TRIGGER IF EXISTS).

-- =====================================================================
-- 0. 공통: updated_at 트리거
-- =====================================================================
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS trigger AS $$
BEGIN NEW.updated_at = now(); RETURN NEW; END;
$$ LANGUAGE plpgsql;

-- 안전한 text→inet 변환: 잘못된 값은 22P02를 던지는 대신 NULL을 반환한다. assets.ip는 자유 텍스트이며
-- (Agent / 자산 API가 호스트명을 써 넣을 수 있다). 그대로 a.ip::inet으로 변환하면 잘못된 데이터 한 줄이 기업 귀속
-- 재계산 구문 전체를 실패시킨다. 호출 측은 try_inet(...) IS NULL로 해당 행을 찾아 경고한다.
-- pg_input_is_valid를 쓰지 않는 이유는 PG16 이상이 필요하기 때문이며, 여기서는 더 오래된 기존 DB와도 호환되어야 한다.
CREATE OR REPLACE FUNCTION try_inet(value text) RETURNS inet AS $$
BEGIN
    RETURN value::inet;
EXCEPTION WHEN others THEN
    RETURN NULL;
END;
$$ LANGUAGE plpgsql IMMUTABLE STRICT;

-- =====================================================================
-- A. 자산 계층: companies / assets / company_scope
-- =====================================================================

CREATE TABLE IF NOT EXISTS companies (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL,
    nkey       TEXT NOT NULL UNIQUE,
    logo       TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_companies_nkey ON companies(nkey);
DROP TRIGGER IF EXISTS trg_companies_upd ON companies;
CREATE TRIGGER trg_companies_upd BEFORE UPDATE ON companies
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS assets (
    id              BIGSERIAL PRIMARY KEY,
    type            TEXT NOT NULL CHECK (type IN (
                        'root_domain','ip','subdomain','app','service','endpoint'
                    )),
    company_id      BIGINT REFERENCES companies(id) ON DELETE SET NULL,
    -- explicit: caller/user selected the company; scope: derived from company_scope.
    -- Existing installations are conservatively migrated as explicit so a scope
    -- rebuild can never erase a historical manual association.
    company_source  TEXT NOT NULL DEFAULT 'explicit'
                    CHECK (company_source IN ('explicit','scope')),
    task_ids        BIGINT[] NOT NULL DEFAULT '{}',
    domain          TEXT,
    root_domain     TEXT,
    ip              TEXT,
    c_segment       CIDR,
    port            INTEGER CHECK (port BETWEEN 1 AND 65535),
    icp             TEXT,
    bound_domains   TEXT[]  NOT NULL DEFAULT '{}',
    open_ports      JSONB[] NOT NULL DEFAULT '{}',
    record_type     TEXT,
    record_value    TEXT[],
    bundle_id       TEXT,
    app_name        TEXT,
    category        TEXT,
    app_description TEXT,
    app_icp         TEXT,
    url             TEXT,
    service_type    TEXT CHECK (service_type IN ('http','other')),
    service_name    TEXT,
    favicon_mmh3    TEXT,
    status_code     INTEGER,
    content_length  BIGINT,
    page_title      TEXT,
    technologies    TEXT[]  NOT NULL DEFAULT '{}',
    auth            JSONB[] NOT NULL DEFAULT '{}',
    method          TEXT,
    params          JSONB[] NOT NULL DEFAULT '{}',
    extra           JSONB   NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_root_domain  ON assets(domain) WHERE type = 'root_domain';
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_ip           ON assets(ip)     WHERE type = 'ip';
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_subdomain    ON assets(domain, COALESCE(record_type,'')) WHERE type = 'subdomain';
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_app_bundle   ON assets(bundle_id) WHERE type = 'app' AND bundle_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_app_name     ON assets(app_name)  WHERE type = 'app' AND bundle_id IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_service_http ON assets(url) WHERE type = 'service' AND service_type = 'http';
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_service_other
    ON assets(COALESCE(domain,''), COALESCE(ip,''), port, service_name) WHERE type = 'service' AND service_type = 'other';
CREATE UNIQUE INDEX IF NOT EXISTS uq_av2_endpoint     ON assets(url, method) WHERE type = 'endpoint';
CREATE INDEX IF NOT EXISTS idx_av2_company      ON assets(company_id)       WHERE company_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_av2_company_type ON assets(company_id, type) WHERE company_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_av2_task_ids     ON assets USING GIN(task_ids);
CREATE INDEX IF NOT EXISTS idx_av2_domain       ON assets(domain)      WHERE domain IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_av2_root_domain  ON assets(root_domain) WHERE root_domain IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_av2_ip           ON assets(ip)          WHERE ip IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_av2_c_segment    ON assets USING GIST(c_segment inet_ops) WHERE c_segment IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_av2_technologies ON assets USING GIN(technologies) WHERE type = 'service';
CREATE INDEX IF NOT EXISTS idx_av2_bound_domains ON assets USING GIN(bound_domains) WHERE type = 'ip';
CREATE INDEX IF NOT EXISTS idx_av2_open_ports   ON assets USING GIN(open_ports)    WHERE type = 'ip';
CREATE INDEX IF NOT EXISTS idx_av2_last_seen    ON assets(last_seen DESC);
CREATE INDEX IF NOT EXISTS idx_av2_type_seen    ON assets(type, last_seen DESC);
ALTER TABLE assets ADD COLUMN IF NOT EXISTS company_source TEXT;
UPDATE assets SET company_source = 'explicit' WHERE company_source IS NULL;
ALTER TABLE assets ALTER COLUMN company_source SET DEFAULT 'explicit';
ALTER TABLE assets ALTER COLUMN company_source SET NOT NULL;
ALTER TABLE assets DROP CONSTRAINT IF EXISTS assets_company_source_check;
ALTER TABLE assets ADD CONSTRAINT assets_company_source_check
    CHECK (company_source IN ('explicit','scope'));
DROP TRIGGER IF EXISTS trg_av2_upd ON assets;
CREATE TRIGGER trg_av2_upd BEFORE UPDATE ON assets
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS company_scope (
    id         BIGSERIAL PRIMARY KEY,
    company_id BIGINT NOT NULL REFERENCES companies(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL CHECK (kind IN ('domain','ip','cidr','icp','keyword')),
    domain     TEXT,
    net        CIDR,
    value      TEXT,
    raw        TEXT NOT NULL,
    reason     TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_sv2_domain UNIQUE (company_id, domain),
    CONSTRAINT uq_sv2_net    UNIQUE (company_id, net),
    CONSTRAINT ck_company_scope_payload CHECK (
        (kind = 'domain' AND domain IS NOT NULL AND net IS NULL AND value IS NULL)
        OR (kind IN ('ip','cidr') AND domain IS NULL AND net IS NOT NULL AND value IS NULL)
        OR (kind IN ('icp','keyword') AND domain IS NULL AND net IS NULL AND value IS NOT NULL)
    )
);
-- Existing installations need the new text payload and expanded kind check.
ALTER TABLE company_scope ADD COLUMN IF NOT EXISTS value TEXT;
ALTER TABLE company_scope DROP CONSTRAINT IF EXISTS company_scope_kind_check;
ALTER TABLE company_scope ADD CONSTRAINT company_scope_kind_check
    CHECK (kind IN ('domain','ip','cidr','icp','keyword'));
ALTER TABLE company_scope DROP CONSTRAINT IF EXISTS ck_company_scope_payload;
ALTER TABLE company_scope ADD CONSTRAINT ck_company_scope_payload CHECK (
    (kind = 'domain' AND domain IS NOT NULL AND net IS NULL AND value IS NULL)
    OR (kind IN ('ip','cidr') AND domain IS NULL AND net IS NOT NULL AND value IS NULL)
    OR (kind IN ('icp','keyword') AND domain IS NULL AND net IS NULL AND value IS NOT NULL)
);
CREATE INDEX IF NOT EXISTS idx_sv2_domain  ON company_scope(domain)   WHERE kind = 'domain';
CREATE INDEX IF NOT EXISTS idx_sv2_net     ON company_scope USING GIST(net inet_ops) WHERE kind IN ('ip','cidr');
CREATE UNIQUE INDEX IF NOT EXISTS uq_sv2_value ON company_scope(company_id, kind, value) WHERE kind IN ('icp','keyword');
CREATE INDEX IF NOT EXISTS idx_sv2_icp ON company_scope(value) WHERE kind = 'icp';
CREATE INDEX IF NOT EXISTS idx_sv2_company ON company_scope(company_id);

-- =====================================================================
-- B. 추론 탐색 계층
-- =====================================================================
CREATE TABLE IF NOT EXISTS explorations (
    id          BIGSERIAL PRIMARY KEY,
    description TEXT,
    goal        TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'open'
                  CHECK (status IN ('open','achieved','failed')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- cold-digest (§2.3): per-task planner round counter — bumped once each time the
-- planner wakes and processes a round. Drives the ≥R cold-node debounce (measured in
-- this exploration's own rounds, not global node ids or wall-clock).
ALTER TABLE explorations ADD COLUMN IF NOT EXISTS round_no BIGINT NOT NULL DEFAULT 0;
DROP TRIGGER IF EXISTS trg_exp_upd ON explorations;
CREATE TRIGGER trg_exp_upd BEFORE UPDATE ON explorations
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS exploration_nodes (
    id             BIGSERIAL PRIMARY KEY,
    exploration_id BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
    kind           TEXT NOT NULL,
    payload        JSONB NOT NULL DEFAULT '{}',
    priority       INT  NOT NULL DEFAULT 0,
    state          TEXT NOT NULL DEFAULT 'open',
    origin         TEXT,
    owner          TEXT,
    blocked_reason TEXT,
    delete_reason  TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at   TIMESTAMPTZ,
    CONSTRAINT ck_node_kind CHECK (kind IN ('begin','goal','intent','fact','finding','hint','digest')),
    CONSTRAINT ck_node_state CHECK (
        (kind='begin'   AND state IN ('open')) OR
        (kind='intent'  AND state IN ('open','running','paused','done','blocked','exhausted','stopped','deleted')) OR
        (kind='goal'    AND state IN ('open','met','abandoned')) OR
        (kind='fact'    AND state IN ('confirmed','dismissed','origin')) OR
        (kind='finding' AND state IN ('confirmed','dismissed')) OR
        (kind='hint'    AND state IN ('active','consumed')) OR
        (kind='digest'  AND state IN ('active','superseded'))
    )
);
ALTER TABLE exploration_nodes ADD COLUMN IF NOT EXISTS blocked_reason TEXT;
-- 의도 소프트 삭제(soft delete): state='deleted'일 때 delete_reason에 사용자가 입력한 삭제 사유를 기록한다.
ALTER TABLE exploration_nodes ADD COLUMN IF NOT EXISTS delete_reason TEXT;
-- cold-digest (§2.3/§5.3): content_version bumps on any change that could alter a
-- digest body (summary/state/confidence); cold_since_round stamps the planner round
-- a node most recently went from "has a live downstream branch" to none (NULL = hot).
ALTER TABLE exploration_nodes ADD COLUMN IF NOT EXISTS content_version  INT    NOT NULL DEFAULT 0;
ALTER TABLE exploration_nodes ADD COLUMN IF NOT EXISTS cold_since_round BIGINT;
-- ck_node_kind: existing installs predate the 'digest' kind — recreate to allow it.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid='exploration_nodes'::regclass
          AND conname='ck_node_kind'
          AND pg_get_constraintdef(oid) NOT LIKE '%digest%'
    ) THEN
        ALTER TABLE exploration_nodes DROP CONSTRAINT ck_node_kind;
        ALTER TABLE exploration_nodes ADD CONSTRAINT ck_node_kind
            CHECK (kind IN ('begin','goal','intent','fact','finding','hint','digest'));
    END IF;
END $$;
-- ck_node_state: recreate when it lacks the 'paused' (older), 'superseded' (digest rev),
-- or 'deleted' (intent soft-delete rev) branches.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid='exploration_nodes'::regclass
          AND conname='ck_node_state'
          AND (pg_get_constraintdef(oid) NOT LIKE '%paused%'
               OR pg_get_constraintdef(oid) NOT LIKE '%superseded%'
               OR pg_get_constraintdef(oid) NOT LIKE '%deleted%')
    ) THEN
        ALTER TABLE exploration_nodes DROP CONSTRAINT ck_node_state;
        ALTER TABLE exploration_nodes ADD CONSTRAINT ck_node_state CHECK (
            (kind='begin'   AND state IN ('open')) OR
            (kind='intent'  AND state IN ('open','running','paused','done','blocked','exhausted','stopped','deleted')) OR
            (kind='goal'    AND state IN ('open','met','abandoned')) OR
            (kind='fact'    AND state IN ('confirmed','dismissed','origin')) OR
            (kind='finding' AND state IN ('confirmed','dismissed')) OR
            (kind='hint'    AND state IN ('active','consumed')) OR
            (kind='digest'  AND state IN ('active','superseded'))
        );
    END IF;
END $$;
CREATE INDEX IF NOT EXISTS idx_expnodes_part     ON exploration_nodes(exploration_id, kind);
CREATE INDEX IF NOT EXISTS idx_expnodes_frontier ON exploration_nodes(exploration_id, priority DESC)
    WHERE kind='intent' AND state='open';
DROP TRIGGER IF EXISTS trg_expnodes_upd ON exploration_nodes;
CREATE TRIGGER trg_expnodes_upd BEFORE UPDATE ON exploration_nodes
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS exploration_edges (
    exploration_id BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
    src_id         BIGINT NOT NULL REFERENCES exploration_nodes(id) ON DELETE CASCADE,
    dst_id         BIGINT NOT NULL REFERENCES exploration_nodes(id) ON DELETE CASCADE,
    rel            TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (exploration_id, src_id, rel, dst_id),
    CONSTRAINT ck_edge_noself CHECK (src_id <> dst_id),
    CONSTRAINT ck_edge_rel CHECK (rel IN ('spawns','derived_from','yields','proves','covers'))
);
CREATE INDEX IF NOT EXISTS idx_expedges_src ON exploration_edges(src_id, rel);
CREATE INDEX IF NOT EXISTS idx_expedges_dst ON exploration_edges(dst_id, rel);
-- cold-digest (§1): the 'covers' relation (digest→member) postdates shipped installs,
-- whose rel CHECK is an inline auto-named constraint. Find and recreate it as ck_edge_rel.
DO $$
DECLARE cname text;
BEGIN
    SELECT conname INTO cname FROM pg_constraint
     WHERE conrelid='exploration_edges'::regclass AND contype='c'
       AND pg_get_constraintdef(oid) LIKE '%rel%'
       AND pg_get_constraintdef(oid) NOT LIKE '%covers%';
    IF cname IS NOT NULL THEN
        EXECUTE 'ALTER TABLE exploration_edges DROP CONSTRAINT '||quote_ident(cname);
        ALTER TABLE exploration_edges ADD CONSTRAINT ck_edge_rel
            CHECK (rel IN ('spawns','derived_from','yields','proves','covers'));
    END IF;
END $$;

CREATE TABLE IF NOT EXISTS exploration_anchors (
    node_id   BIGINT NOT NULL REFERENCES exploration_nodes(id) ON DELETE CASCADE,
    asset_id  BIGINT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    PRIMARY KEY (node_id, asset_id)
);
CREATE INDEX IF NOT EXISTS idx_anchor_asset ON exploration_anchors(asset_id);

-- task_constraints: operator-authored operation constraints (allow/deny) for a task.
-- Extracted by the goals decomposer at round 0 (from goal/description), editable at
-- 런타임에 메인 에이전트와 개요 화면의 「제약 관리」에서 편집할 수 있다. 라운드마다 planner/worker 시스템
-- prompt each round (config-gated) to keep exploration within the operator's boundary.
CREATE TABLE IF NOT EXISTS task_constraints (
    id             BIGSERIAL PRIMARY KEY,
    exploration_id BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
    kind           TEXT NOT NULL CHECK (kind IN ('allow','deny')),
    text           TEXT NOT NULL,
    origin         TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_task_constraints_exp ON task_constraints(exploration_id);

CREATE TABLE IF NOT EXISTS activity (
    id                 BIGSERIAL PRIMARY KEY,
    exploration_id     BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
    node_id            BIGINT REFERENCES exploration_nodes(id) ON DELETE SET NULL,
    worker             TEXT,
    kind               TEXT,
    tool               TEXT,
    tool_use_id        TEXT,
    is_error           BOOLEAN NOT NULL DEFAULT false,
    summary            TEXT,
    detail             TEXT,
    metadata           JSONB NOT NULL DEFAULT '{}',
    input_tokens       INTEGER,
    output_tokens      INTEGER,
    cache_read_tokens  INTEGER,
    cache_write_tokens INTEGER,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE activity ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}';
-- main_seg segments the main-agent session into resettable conversations: a new
-- main session bumps the segment so its transcript + activity start clean while the
-- task's graph/assets/goal are untouched. NULL == legacy rows == segment 0 (the
-- original session). Only worker='mainagent' rows carry it.
ALTER TABLE activity ADD COLUMN IF NOT EXISTS main_seg INTEGER;
CREATE INDEX IF NOT EXISTS idx_act_node  ON activity(exploration_id, node_id, id);
CREATE INDEX IF NOT EXISTS idx_act_since ON activity(exploration_id, id);
CREATE INDEX IF NOT EXISTS idx_act_tool_call ON activity(exploration_id, tool_use_id, id)
  WHERE kind IN ('tool_use', 'tool_result');
-- Main/Plan history pages filter by worker (both carry NULL node_id, so idx_act_node
-- can't distinguish them); this covers reverse pagination of those sessions.
CREATE INDEX IF NOT EXISTS idx_act_worker ON activity(exploration_id, worker, id);
-- Main-session pages filter by segment on top of worker='mainagent'; this partial
-- index covers reverse pagination within one segment.
CREATE INDEX IF NOT EXISTS idx_act_main_seg ON activity(exploration_id, main_seg, id)
    WHERE worker='mainagent';
-- Task-list polls aggregate result usage and find the latest event repeatedly.
-- Cover the token columns for index-only aggregation and the timestamp order for
-- per-exploration latest-activity lookups.
CREATE INDEX IF NOT EXISTS idx_act_result_usage ON activity(exploration_id)
    INCLUDE (input_tokens, output_tokens, cache_read_tokens, cache_write_tokens)
    WHERE kind='result';
CREATE INDEX IF NOT EXISTS idx_act_latest ON activity(exploration_id, created_at DESC);

-- main_sessions records the resettable main-agent conversation segments of a task.
-- Segment 0 (the original session) is implicit and never stored; this table holds
-- "새 세션"으로 만들어진 추가 세그먼트만 저장한다(seq >= 1). 현재 세그먼트는
-- MAX(seq) or 0. Each segment gets its own transcript file + activity slice; the
-- task's exploration graph/assets/goal are shared and never reset.
CREATE TABLE IF NOT EXISTS main_sessions (
    exploration_id BIGINT NOT NULL REFERENCES explorations(id) ON DELETE CASCADE,
    seq            INTEGER NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (exploration_id, seq)
);

-- =====================================================================
-- C. LLM profiles
-- =====================================================================
CREATE TABLE IF NOT EXISTS settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS llm_profiles (
    id               BIGSERIAL PRIMARY KEY,
    name             TEXT NOT NULL UNIQUE,
    format           TEXT NOT NULL CHECK (format IN ('openai','anthropic','openai-responses')),
    base_url         TEXT,
    proxy            TEXT,
    model            TEXT NOT NULL,
    api_key          TEXT,
    api_key_hint     TEXT,
    rate_per_second  DOUBLE PRECISION NOT NULL DEFAULT 0,
    rate_per_minute  DOUBLE PRECISION NOT NULL DEFAULT 0,
    context_window_k INTEGER NOT NULL DEFAULT 0,
    -- 사고 파라미터는 두 개의 독립 필드로 나뉜다: thinking_type=사고 스위치(''/disabled/enabled),
    -- reasoning_effort=사고 강도(''/low/medium/high/xhigh/max), 서로 영향을 주지 않는다.
    reasoning_effort TEXT NOT NULL DEFAULT '',
    thinking_type    TEXT NOT NULL DEFAULT '',
    is_default       BOOLEAN NOT NULL DEFAULT false,
    -- 폴링(장애 조치) 파라미터, 참고: docs/LLM轮询设计.md:
    --   priority     순위. 값이 클수록 먼저 선택되며, 활성 구성(is_default)은 항상 체인 맨 앞에 오고 이 값과 무관하다.
    --   pool_exclude true=장애 조치 대상에서 제외(에이전트/태스크에서 명시적으로 바인딩하는 것은 여전히 가능).
    priority         INTEGER NOT NULL DEFAULT 0,
    pool_exclude     BOOLEAN NOT NULL DEFAULT false,
    -- streaming=true(기본값)이면 스트리밍 SSE를 사용하고, false이면 진짜 비스트리밍(stream:false, 한 번에 받는 JSON)을 사용한다.
    streaming        BOOLEAN NOT NULL DEFAULT true,
    -- 단일 응답의 출력 상한(토큰). 0=이 필드를 보내지 않고 서버 기본값에 따른다. 기존 동작을 유지하기 위함이다.
    -- context_window_k(모델 총 용량, 로컬에서만 압축 임계값에 사용)와는 별개다. 이 값은 요청에 실제로 전송된다.
    max_tokens       INTEGER NOT NULL DEFAULT 0,
    -- 출력 상한의 요청 필드명은 format='openai'일 때만 적용된다:
    --   ''                      = max_tokens(기본값, 대부분의 게이트웨이와 호환)
    --   'max_completion_tokens' = 새 필드. OpenAI 추론 모델(o 시리즈/GPT-5)은 이것만 인식하며,
    --                             max_tokens를 보내면 unsupported_parameter로 거부된다.
    -- anthropic(max_tokens 필수)과 openai-responses(max_output_tokens)는 자체 필드명을 쓰므로 이 값의 영향을 받지 않는다.
    max_tokens_field TEXT NOT NULL DEFAULT '',
    -- 커스텀 세션 헤더: 비어 있지 않으면 매 요청마다 해당 이름의 HTTP 헤더를 보내며, 헤더 값은 현재 실행 중인 session id이다
    -- (chat 세션/worker 의도). session-id 헤더로 프롬프트 캐시/스티키 라우팅을 하는 일부 게이트웨이용이다. ''이면 보내지 않는다.
    session_header_key TEXT NOT NULL DEFAULT '',
    -- 재시도 오버라이드: 횟수 0=전역 기본값 사용/-1=끔/>0=해당 값; 간격 0=기본 지수 백오프 사용/>0=고정 밀리초.
    -- 세 그룹은 각각 연결 재시도, 빈 응답 재시도, 같은 provider 안전 윈도우 재시도에 대응하며, 자세한 내용은 아래 ALTER 부분의 주석을 참고한다.
    retry_connect_attempts    INTEGER NOT NULL DEFAULT 0,
    retry_connect_interval_ms INTEGER NOT NULL DEFAULT 0,
    retry_empty_attempts      INTEGER NOT NULL DEFAULT 0,
    retry_empty_interval_ms   INTEGER NOT NULL DEFAULT 0,
    retry_stream_attempts     INTEGER NOT NULL DEFAULT 0,
    retry_stream_interval_ms  INTEGER NOT NULL DEFAULT 0,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_llm_one_default ON llm_profiles(is_default) WHERE is_default;
DROP TRIGGER IF EXISTS trg_llm_upd ON llm_profiles;
CREATE TRIGGER trg_llm_upd BEFORE UPDATE ON llm_profiles
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
-- 폴링 순위/제외 표시; 기존 DB 보완. 기본값 0 / false = 모든 구성이 폴링에 참여한다.
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS priority     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS pool_exclude BOOLEAN NOT NULL DEFAULT false;
-- 스트리밍 스위치; 기존 DB 보완. 기본값 true = 기존 스트리밍 동작을 유지하여 기존 구성은 체감 없이 업그레이드된다.
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS streaming    BOOLEAN NOT NULL DEFAULT true;
-- format 제약을 풀어 openai-responses(OpenAI Responses API)를 수용한다; 기존 DB 보완.
-- 매 시작 시 실행되며 멱등하다: 기존 CHECK를 먼저 삭제한 뒤 세 가지 값을 포함한 새 CHECK를 만든다.
ALTER TABLE llm_profiles DROP CONSTRAINT IF EXISTS llm_profiles_format_check;
ALTER TABLE llm_profiles ADD  CONSTRAINT llm_profiles_format_check
    CHECK (format IN ('openai','anthropic','openai-responses'));

-- 출력 상한 및 그 필드명; 기존 DB 보완. 기본값 0 / '' = 상한을 보내지 않고 max_tokens 필드명을 그대로 사용하며,
-- 기존 구성의 동작은 완전히 그대로이다.
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS max_tokens       INTEGER NOT NULL DEFAULT 0;
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS max_tokens_field TEXT    NOT NULL DEFAULT '';
-- 같은 format: 먼저 삭제한 뒤 다시 만들어 매 시작마다 멱등성을 보장한다.
ALTER TABLE llm_profiles DROP CONSTRAINT IF EXISTS llm_profiles_max_tokens_field_check;
ALTER TABLE llm_profiles ADD  CONSTRAINT llm_profiles_max_tokens_field_check
    CHECK (max_tokens_field IN ('','max_completion_tokens'));
ALTER TABLE llm_profiles DROP CONSTRAINT IF EXISTS llm_profiles_max_tokens_check;
ALTER TABLE llm_profiles ADD  CONSTRAINT llm_profiles_max_tokens_check
    CHECK (max_tokens >= 0);
-- 커스텀 세션 헤더 이름; 기존 DB 보완. 기본값 '' = 보내지 않으며, 기존 구성의 동작은 그대로이다.
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS session_header_key TEXT NOT NULL DEFAULT '';

-- 단일 구성의 재시도 오버라이드(docs/LLM重试设计.md 참고). 세 그룹 각각이 「횟수 + 고정 간격」 쌍을 가지며,
-- 의미는 통일되어 있다: 횟수 0=전역 기본값 사용, -1=해당 계층의 재시도 끔, >0=해당 값 사용; 간격 0=해당 계층의 기본 지수 백오프 사용, >0=이 고정 밀리초 사용.
-- 모두 기본값 0이므로 기존 DB/구성의 동작은 바뀌지 않는다.
--   connect = 연결 재시도(SDK doStream: 연결 리셋/타임아웃/429/5xx, 스트림 시작 전)
--   empty   = 빈 응답 재시도(SDK: 완료되었지만 content block이 하나도 없는 경우, openai 형식에서만)
--   stream  = 같은 provider 안전 윈도우 재시도(본 프로젝트 task_llm: 출력을 전달하기 전 스트림 끊김 시 재생)
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS retry_connect_attempts    INTEGER NOT NULL DEFAULT 0;
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS retry_connect_interval_ms INTEGER NOT NULL DEFAULT 0;
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS retry_empty_attempts      INTEGER NOT NULL DEFAULT 0;
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS retry_empty_interval_ms   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS retry_stream_attempts     INTEGER NOT NULL DEFAULT 0;
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS retry_stream_interval_ms  INTEGER NOT NULL DEFAULT 0;
-- 같은 format: 먼저 삭제한 뒤 다시 만들어 매 시작마다 멱등성을 보장한다. 횟수 하한은 -1(끔), 간격은 음수가 될 수 없다.
ALTER TABLE llm_profiles DROP CONSTRAINT IF EXISTS llm_profiles_retry_check;
ALTER TABLE llm_profiles ADD  CONSTRAINT llm_profiles_retry_check CHECK (
    retry_connect_attempts >= -1 AND retry_empty_attempts >= -1 AND retry_stream_attempts >= -1
    AND retry_connect_interval_ms >= 0 AND retry_empty_interval_ms >= 0 AND retry_stream_interval_ms >= 0);

-- 사고 스위치 필드 thinking_type은 기존의 단일 reasoning_effort 의미에서 한 번에 분리해 온 것이다.
-- schema.sql은 매 시작마다 실행되므로 마이그레이션은 한 번만 실행되어야 한다. 해당 컬럼이 아직 없을 때만 백필하며,
-- 그렇지 않으면 사용자가 나중에 수동으로 설정한 조합이 매 시작마다 되돌아간다. 기존 reasoning_effort의 의미는 다음과 같다:
--   'off'                    → 명시적 끔 → thinking_type='disabled', 강도는 비움
--   'low/medium/high/max'    → 켬 + 강도 → thinking_type='enabled', 강도 유지
--   ''                       → 보내지 않음 → 둘 다 비움(기본값)
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_name = 'llm_profiles' AND column_name = 'thinking_type'
    ) THEN
        ALTER TABLE llm_profiles ADD COLUMN thinking_type TEXT NOT NULL DEFAULT '';
        UPDATE llm_profiles SET thinking_type = 'enabled'
            WHERE reasoning_effort IN ('low','medium','high','max');
        UPDATE llm_profiles SET thinking_type = 'disabled', reasoning_effort = ''
            WHERE reasoning_effort = 'off';
    END IF;
END $$;

-- LLM 폴링 서킷 브레이커 상태: 어떤 구성이 연속으로 실패(잔액 부족/키 무효/속도 제한)하면 쿨다운에 들어가며, 쿨다운 기간 동안
-- 폴링은 해당 구성을 바로 건너뛴다. 메모리 상태가 기준이며, 여기서 DB에 저장하는 것은 재시작 후에도 쿨다운 구간을 잃지 않기 위해서다. 로드 시에는 아직 만료되지 않은 행만 가져오고(open_until > now),
-- 만료된 행은 자연스럽게 「정상」 상태로 돌아가며, 다음 호출 때 half-open 시험을 한다.
CREATE TABLE IF NOT EXISTS llm_profile_health (
    profile_id  BIGINT PRIMARY KEY REFERENCES llm_profiles(id) ON DELETE CASCADE,
    fails       INTEGER NOT NULL DEFAULT 0,  -- 현재 연속 실패 횟수(성공 시 0으로 초기화)
    trips       INTEGER NOT NULL DEFAULT 0,  -- 누적 서킷 차단 횟수, 쿨다운 시간의 지수 백오프에 사용
    open_until  TIMESTAMPTZ,                 -- 쿨다운 종료 시각; NULL/만료 = 차단되지 않음
    last_error  TEXT NOT NULL DEFAULT '',
    last_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- =====================================================================
-- D. 태스크 계층
-- =====================================================================
-- Global task categories are intentionally independent from task templates.
-- Deleting a category only moves its tasks back to the uncategorized bucket.
CREATE TABLE IF NOT EXISTS task_categories (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL,
    nkey       TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_task_categories_name ON task_categories(name, id);
DROP TRIGGER IF EXISTS trg_task_categories_upd ON task_categories;
CREATE TRIGGER trg_task_categories_upd BEFORE UPDATE ON task_categories
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS tasks (
    id             BIGSERIAL PRIMARY KEY,
    name           TEXT NOT NULL DEFAULT '',
    category_id    BIGINT REFERENCES task_categories(id) ON DELETE SET NULL,
    description    TEXT NOT NULL,
    goal           TEXT NOT NULL,
    exploration_id BIGINT NOT NULL UNIQUE
                     REFERENCES explorations(id) ON DELETE RESTRICT,
    status         TEXT NOT NULL DEFAULT 'created'
                     CHECK (status IN ('created','running','paused','done','failed','timeout')),
    paused         BOOLEAN NOT NULL DEFAULT false,
    queued         BOOLEAN NOT NULL DEFAULT false,
    queued_at      TIMESTAMPTZ,
    queue_mode     TEXT NOT NULL DEFAULT '',
    llm_profile_id BIGINT REFERENCES llm_profiles(id) ON DELETE SET NULL,
    active_llm_profile_id BIGINT REFERENCES llm_profiles(id) ON DELETE SET NULL,
    llm_chain_revision BIGINT NOT NULL DEFAULT 0,
    company_id     BIGINT REFERENCES companies(id) ON DELETE SET NULL,
    parent_ref     TEXT,
    timeout_seconds INTEGER NOT NULL DEFAULT 0,
    plan_heartbeat_seconds INTEGER NOT NULL DEFAULT 300,
    coverage_enabled BOOLEAN NOT NULL DEFAULT true,
    pinned_at      TIMESTAMPTZ,
    first_run_at   TIMESTAMPTZ,
    deadline_at    TIMESTAMPTZ,
    archived_at    TIMESTAMPTZ,
    deleted_at     TIMESTAMPTZ,
    completed_at   TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_tasks_alive  ON tasks(created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_tasks_status ON tasks(status)          WHERE deleted_at IS NULL;
DROP TRIGGER IF EXISTS trg_tasks_upd ON tasks;
CREATE TRIGGER trg_tasks_upd BEFORE UPDATE ON tasks
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
-- planner 하트비트 트리거 간격(초); 기존 DB 보완. 기본값 300초(5분). docs/planner-trigger-impl-plan.md 참고
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS plan_heartbeat_seconds INTEGER NOT NULL DEFAULT 300;
-- 동시성 상한 대기 상태; 기존 DB 보완. true = 동시성 상한 때문에 큐에 대기하며 빈 자리가 나면 자동으로 시작된다.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS queued BOOLEAN NOT NULL DEFAULT false;
-- 자산 커버리지 기능 스위치; 기존 DB 보완. true(기본값) = 테스트 커버리지를 계산/표시하고 테스트 범위를 자동으로 누적하며,
-- agent에 add_task_scope/list_untested_assets를 개방한다. false = 모두 끔(task_scope.go 참고).
-- 기존 태스크의 기본값 true는 기존 동작을 유지한다. 기업 연관(task_scope kind=company)은 이 스위치의 영향을 받지 않는다.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS coverage_enabled BOOLEAN NOT NULL DEFAULT true;
-- queued_at makes admission FIFO reflect the actual enqueue order rather than the
-- task creation order. queue_mode distinguishes first bootstrap from resuming an
-- exploration that already owns goals/history.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS queued_at TIMESTAMPTZ;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS queue_mode TEXT NOT NULL DEFAULT '';
-- 선택적 태스크 이름; 기존 DB 보완. 빈 문자열 = 이름 없음, 프론트엔드에서는 설명으로 대체해 표시한다.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS name TEXT NOT NULL DEFAULT '';
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS category_id BIGINT REFERENCES task_categories(id) ON DELETE SET NULL;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS pinned_at TIMESTAMPTZ;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS active_llm_profile_id BIGINT REFERENCES llm_profiles(id) ON DELETE SET NULL;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS llm_chain_revision BIGINT NOT NULL DEFAULT 0;
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_tasks_category ON tasks(category_id, created_at DESC)
    WHERE deleted_at IS NULL AND category_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_tasks_pinned ON tasks(pinned_at DESC)
    WHERE deleted_at IS NULL AND pinned_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_tasks_archived ON tasks(archived_at DESC)
    WHERE archived_at IS NOT NULL;

-- Cold task archives retain only compact metadata in PostgreSQL. The complete
-- task payload lives in a versioned .tar.zst package under data/archives/tasks.
-- task_id stays unique so an operation can be retried safely after a restart.
CREATE TABLE IF NOT EXISTS task_archives (
    id                         BIGSERIAL PRIMARY KEY,
    task_id                    BIGINT NOT NULL UNIQUE REFERENCES tasks(id) ON DELETE CASCADE,
    state                      TEXT NOT NULL DEFAULT 'archive_queued' CHECK (state IN (
                                   'archive_queued','archiving','archive_failed','ready',
                                   'restore_queued','restoring','restore_failed',
                                   'delete_queued','deleting','delete_failed'
                               )),
    phase                      TEXT NOT NULL DEFAULT 'queued',
    progress                   INTEGER NOT NULL DEFAULT 0 CHECK (progress BETWEEN 0 AND 100),
    error                      TEXT NOT NULL DEFAULT '',
    warnings                   JSONB NOT NULL DEFAULT '[]',
    format_version             INTEGER NOT NULL DEFAULT 2,
    archive_path               TEXT NOT NULL DEFAULT '',
    sha256                     TEXT NOT NULL DEFAULT '',
    original_size              BIGINT NOT NULL DEFAULT 0,
    compressed_size            BIGINT NOT NULL DEFAULT 0,
    task_name                  TEXT NOT NULL DEFAULT '',
    task_description           TEXT NOT NULL DEFAULT '',
    task_goal                  TEXT NOT NULL DEFAULT '',
    original_status            TEXT NOT NULL DEFAULT '',
    category_id_snapshot       BIGINT,
    category_name_snapshot     TEXT NOT NULL DEFAULT '',
    source_task_ids            BIGINT[] NOT NULL DEFAULT '{}',
    remaining_timeout_seconds  BIGINT NOT NULL DEFAULT 0,
    data_counts                JSONB NOT NULL DEFAULT '{}',
    aggregate_stats            JSONB NOT NULL DEFAULT '{}',
    archived_at                TIMESTAMPTZ,
    requested_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                 TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE task_archives ALTER COLUMN format_version SET DEFAULT 2;
CREATE INDEX IF NOT EXISTS idx_task_archives_state ON task_archives(state, requested_at, id);
CREATE INDEX IF NOT EXISTS idx_task_archives_archived ON task_archives(archived_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_task_archives_sources ON task_archives USING GIN(source_task_ids);
DROP TRIGGER IF EXISTS trg_task_archives_upd ON task_archives;
CREATE TRIGGER trg_task_archives_upd BEFORE UPDATE ON task_archives
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Reusable task description/goal presets. nkey is the normalized, case-insensitive
-- identity used to reject visually equivalent duplicate names.
CREATE TABLE IF NOT EXISTS task_templates (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL,
    nkey        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL,
    goal        TEXT NOT NULL,
    -- 프리셋의 태스크 분류; 분류 삭제 시 NULL로 설정된다(tasks.category_id와 일치, 차단하지 않음).
    category_id     BIGINT REFERENCES task_categories(id) ON DELETE SET NULL,
    -- 프리셋의 태스크 레벨 차단/허용 규칙 스냅샷(AssetInterceptRuleInput 배열); 템플릿 적용 시 새 태스크에 주입된다.
    intercept_rules JSONB NOT NULL DEFAULT '[]',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- 기존 DB 보완(이미 배포됨, IF NOT EXISTS와 함께 컬럼 추가).
ALTER TABLE task_templates ADD COLUMN IF NOT EXISTS category_id BIGINT REFERENCES task_categories(id) ON DELETE SET NULL;
ALTER TABLE task_templates ADD COLUMN IF NOT EXISTS intercept_rules JSONB NOT NULL DEFAULT '[]';
CREATE INDEX IF NOT EXISTS idx_task_templates_updated ON task_templates(updated_at DESC, id DESC);
DROP TRIGGER IF EXISTS trg_task_templates_upd ON task_templates;
CREATE TRIGGER trg_task_templates_upd BEFORE UPDATE ON task_templates
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Direct, read-only task context inheritance. Relations are intentionally not
-- recursive: a task sees only the source tasks explicitly chosen at creation.
CREATE TABLE IF NOT EXISTS task_relations (
    task_id        BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    source_task_id BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, source_task_id),
    CONSTRAINT ck_task_relation_not_self CHECK (task_id <> source_task_id)
);
CREATE INDEX IF NOT EXISTS idx_task_relations_source ON task_relations(source_task_id);

-- Task/asset provenance supplements the legacy assets.task_ids association. The
-- array remains the compatibility source for existing query and cleanup paths;
-- this relation records how each association was obtained for operator review.
CREATE TABLE IF NOT EXISTS task_asset_links (
    task_id        BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    asset_id       BIGINT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    source         TEXT NOT NULL DEFAULT 'system',
    source_summary TEXT NOT NULL DEFAULT '',
    source_node_id BIGINT REFERENCES exploration_nodes(id) ON DELETE SET NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, asset_id)
);
CREATE INDEX IF NOT EXISTS idx_task_asset_links_asset ON task_asset_links(asset_id, task_id);
CREATE INDEX IF NOT EXISTS idx_task_asset_links_node ON task_asset_links(source_node_id)
    WHERE source_node_id IS NOT NULL;
DROP TRIGGER IF EXISTS trg_task_asset_links_upd ON task_asset_links;
CREATE TRIGGER trg_task_asset_links_upd BEFORE UPDATE ON task_asset_links
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Keep provenance rows synchronized when existing asset upsert paths append or
-- remove task ids. Detailed callers overwrite the generic source after upsert.
CREATE OR REPLACE FUNCTION sync_task_asset_links() RETURNS trigger AS $$
BEGIN
    INSERT INTO task_asset_links(task_id, asset_id, source, source_summary)
    SELECT task.id, NEW.id, 'system', '任务执行期间自动关联'
    FROM unnest(NEW.task_ids) AS requested(task_id)
    JOIN tasks task ON task.id=requested.task_id AND task.deleted_at IS NULL
    ON CONFLICT (task_id, asset_id) DO NOTHING;

    DELETE FROM task_asset_links link
    WHERE link.asset_id=NEW.id
      AND NOT (link.task_id=ANY(NEW.task_ids));
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_assets_task_links ON assets;
CREATE TRIGGER trg_assets_task_links AFTER INSERT OR UPDATE OF task_ids ON assets
    FOR EACH ROW EXECUTE FUNCTION sync_task_asset_links();

-- Existing installations receive an auditable legacy source without rewriting
-- task_ids. Ignore stale array ids that no longer resolve to a live task.
INSERT INTO task_asset_links(task_id, asset_id, source, source_summary)
SELECT task.id, asset.id, 'legacy', '由历史任务资产关联迁移'
FROM assets asset
CROSS JOIN LATERAL unnest(asset.task_ids) AS requested(task_id)
JOIN tasks task ON task.id=requested.task_id AND task.deleted_at IS NULL
ON CONFLICT (task_id, asset_id) DO NOTHING;

-- Ordered task-level LLM failover chain. A quota-exhausted entry is skipped
-- until the user saves/resets the chain, which clears all failure state.
CREATE TABLE IF NOT EXISTS task_llm_profiles (
    task_id          BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    profile_id       BIGINT NOT NULL REFERENCES llm_profiles(id) ON DELETE CASCADE,
    position         INTEGER NOT NULL CHECK (position >= 0),
    status           TEXT NOT NULL DEFAULT 'ready'
                       CHECK (status IN ('ready','quota_exhausted')),
    last_error       TEXT,
    exhausted_at     TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (task_id, profile_id),
    UNIQUE (task_id, position)
);
CREATE INDEX IF NOT EXISTS idx_task_llm_profiles_order ON task_llm_profiles(task_id, position);
CREATE INDEX IF NOT EXISTS idx_task_llm_profiles_profile ON task_llm_profiles(profile_id, task_id);
CREATE INDEX IF NOT EXISTS idx_tasks_llm_profile ON tasks(llm_profile_id) WHERE llm_profile_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_tasks_active_llm_profile ON tasks(active_llm_profile_id) WHERE active_llm_profile_id IS NOT NULL;
DROP TRIGGER IF EXISTS trg_task_llm_profiles_upd ON task_llm_profiles;
CREATE TRIGGER trg_task_llm_profiles_upd BEFORE UPDATE ON task_llm_profiles
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- One-time-compatible backfill: old pinned tasks become one-entry chains. A user
-- can still clear the chain later because the update path also clears the legacy
-- llm_profile_id column, preventing this block from re-adding it on restart.
INSERT INTO task_llm_profiles(task_id, profile_id, position)
SELECT t.id, t.llm_profile_id, 0
FROM tasks t
WHERE t.llm_profile_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM task_llm_profiles x WHERE x.task_id=t.id)
ON CONFLICT DO NOTHING;
UPDATE tasks t
SET active_llm_profile_id = t.llm_profile_id
WHERE t.active_llm_profile_id IS NULL
  AND t.llm_profile_id IS NOT NULL
  AND EXISTS (SELECT 1 FROM task_llm_profiles x WHERE x.task_id=t.id AND x.profile_id=t.llm_profile_id);

-- 태스크 테스트 범위(자산 커버리지의 분모 + 인가 경계).
--   자동 입력(source='auto'): insertAssets 최상위에서 worker별로 명시적으로 삽입되는 자산 유형에 보수적 범위를 추가한다
--     (root_domain→root_domain, subdomain/service/endpoint→subdomain(host), ip→ip)로 보수적 범위를 추가한다.
--     side-effect로 파생된 자산은 범위에 넣지 않는다(훅은 handler 최상위에 있고, 파생은 db 계층 내부에서 일어난다).
--   agent 입력(source='agent'): add_task_scope가 company/root_domain/subdomain/ip를 추가한다.
-- 커버리지 = 매칭되는 active 행의 assets(분모) 중 fact 노드에 의해 고정된 비율(분자).
CREATE TABLE IF NOT EXISTS task_scope (
    id          BIGSERIAL PRIMARY KEY,
    task_id     BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL CHECK (kind IN ('company','root_domain','subdomain','ip','cidr','icp','keyword')),
    company_id  BIGINT REFERENCES companies(id) ON DELETE CASCADE,  -- kind='company'
    domain      TEXT,          -- root_domain / subdomain
    net         CIDR,          -- ip / cidr
    value       TEXT,          -- icp / keyword
    source      TEXT NOT NULL DEFAULT 'auto' CHECK (source IN ('auto','agent','manual')),
    reason      TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- 기존 DB 업그레이드: 태스크 범위를 기업 범위의 단일 텍스트 상자 인식 기능과 일치하도록 확장한다.
ALTER TABLE task_scope ADD COLUMN IF NOT EXISTS value TEXT;
ALTER TABLE task_scope DROP CONSTRAINT IF EXISTS task_scope_kind_check;
ALTER TABLE task_scope ADD CONSTRAINT task_scope_kind_check
    CHECK (kind IN ('company','root_domain','subdomain','ip','cidr','icp','keyword'));
-- 중복 제거: 같은 task의 같은 범위는 한 번만 저장한다(자동 입력의 배치 삽입이 이에 의존하여 멱등하다).
DROP INDEX IF EXISTS uq_task_scope;
CREATE UNIQUE INDEX IF NOT EXISTS uq_task_scope_v2 ON task_scope(
    task_id, kind, COALESCE(domain,''), COALESCE(net::text,''), COALESCE(company_id,0), COALESCE(value,''));
CREATE INDEX IF NOT EXISTS idx_ts_domain  ON task_scope(domain) WHERE kind IN ('root_domain','subdomain');
CREATE INDEX IF NOT EXISTS idx_ts_net     ON task_scope USING GIST(net inet_ops) WHERE kind IN ('ip','cidr');
CREATE INDEX IF NOT EXISTS idx_ts_company ON task_scope(company_id) WHERE kind = 'company';

-- =====================================================================
-- E. Agents / 프롬프트 템플릿 / 변수 카탈로그
-- =====================================================================
CREATE TABLE IF NOT EXISTS agents (
    id                BIGSERIAL PRIMARY KEY,
    key               TEXT NOT NULL UNIQUE CHECK (key ~ '^[a-z][a-z0-9_]*$'),
    name              TEXT NOT NULL,
    description       TEXT,
    role              TEXT NOT NULL,
    builtin           BOOLEAN NOT NULL DEFAULT true,
    enabled           BOOLEAN NOT NULL DEFAULT true,
    llm_profile_id    BIGINT REFERENCES llm_profiles(id) ON DELETE SET NULL,
    current_prompt_id BIGINT,
    max_turns         INTEGER NOT NULL DEFAULT 0,
    run_seconds       INTEGER NOT NULL DEFAULT 1200,
    web_search        BOOLEAN NOT NULL DEFAULT false,
    interactive_shell BOOLEAN NOT NULL DEFAULT false,
    wrapup_prompt     TEXT NOT NULL DEFAULT '',
    wrapup_max_turns  INTEGER NOT NULL DEFAULT 0,
    task_timeout_wrapup_prompt    TEXT NOT NULL DEFAULT '',
    task_timeout_wrapup_max_turns INTEGER NOT NULL DEFAULT 0,
    trigger_run_mode     TEXT    NOT NULL DEFAULT 'serial'  CHECK (trigger_run_mode IN ('serial','parallel')),
    trigger_merge_mode   TEXT    NOT NULL DEFAULT 'all' CHECK (trigger_merge_mode IN ('by_task','all','none')),
    trigger_max_parallel INTEGER NOT NULL DEFAULT 5,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT agents_role_ck CHECK (role IN ('goals','main','planner','worker','assistant'))
);
-- 컬럼 추가 마이그레이션(이미 배포됨, 기존 DB 업그레이드 시 컬럼 보완; 새 DB의 CREATE에는 이미 포함. 마이그레이션에는 CHECK를 붙이지 않음: 기존 데이터 안전성 + 백엔드 쓰기 화이트리스트가 보완).
ALTER TABLE agents ADD COLUMN IF NOT EXISTS trigger_run_mode     TEXT    NOT NULL DEFAULT 'serial';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS trigger_merge_mode   TEXT    NOT NULL DEFAULT 'all';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS trigger_max_parallel INTEGER NOT NULL DEFAULT 5;
-- agent별 LLM 바인딩(agent 레벨 기본 모델): 컬럼은 초기 버전부터 위 CREATE에 있으며, 이 ALTER는 매우 오래된 DB를 위한 폴백일 뿐이다(멱등).
ALTER TABLE agents ADD COLUMN IF NOT EXISTS llm_profile_id BIGINT REFERENCES llm_profiles(id) ON DELETE SET NULL;
-- run_seconds: 단일 run의 벽시계 기본값 600→1200. 컬럼 기본값만 바꾸며(앞으로 삽입되는 행에만 영향), 기존 DB의 기존 행은 건드리지 않는다.
ALTER TABLE agents ALTER COLUMN run_seconds SET DEFAULT 1200;
CREATE INDEX IF NOT EXISTS idx_agents_llm_profile ON agents(llm_profile_id) WHERE llm_profile_id IS NOT NULL;
DROP TRIGGER IF EXISTS trg_agents_upd ON agents;
CREATE TRIGGER trg_agents_upd BEFORE UPDATE ON agents
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS agent_prompts (
    id            BIGSERIAL PRIMARY KEY,
    agent_id      BIGINT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    version       INT NOT NULL,
    template_text TEXT NOT NULL,
    note          TEXT,
    updated_by    TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (agent_id, version)
);
-- 순환 외래키: agents.current_prompt_id → agent_prompts.id (두 테이블 생성 후에 추가해야 함)
DO $$ BEGIN
    ALTER TABLE agents ADD CONSTRAINT fk_agents_curprompt
        FOREIGN KEY (current_prompt_id) REFERENCES agent_prompts(id) ON DELETE SET NULL;
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

CREATE TABLE IF NOT EXISTS agent_prompt_vars (
    id          BIGSERIAL PRIMARY KEY,
    agent_id    BIGINT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    var_name    TEXT NOT NULL,
    description TEXT,
    example     TEXT,
    source      TEXT NOT NULL CHECK (source IN ('exploration','runtime','distilled')),
    UNIQUE (agent_id, var_name)
);

-- =====================================================================
-- F. MCP 서비스
-- =====================================================================
CREATE TABLE IF NOT EXISTS mcp_servers (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    transport   TEXT NOT NULL CHECK (transport IN ('stdio','http','sse')),
    command     TEXT,
    args        JSONB NOT NULL DEFAULT '[]',
    env         JSONB NOT NULL DEFAULT '{}',
    url         TEXT,
    enabled     BOOLEAN NOT NULL DEFAULT true,
    insecure    BOOLEAN NOT NULL DEFAULT false,  -- http: TLS 인증서 검증 건너뛰기(자체 서명 인증서 시나리오, issue #108)
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Allow legacy MCP SSE servers on databases created before SSE support.
ALTER TABLE mcp_servers DROP CONSTRAINT IF EXISTS mcp_servers_transport_check;
ALTER TABLE mcp_servers ADD CONSTRAINT mcp_servers_transport_check
    CHECK (transport IN ('stdio','http','sse'));
-- 기존 DB 컬럼 보완(schema.sql은 매 시작마다 Exec된다).
ALTER TABLE mcp_servers ADD COLUMN IF NOT EXISTS insecure BOOLEAN NOT NULL DEFAULT false;
DROP TRIGGER IF EXISTS trg_mcp_upd ON mcp_servers;
CREATE TRIGGER trg_mcp_upd BEFORE UPDATE ON mcp_servers
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 기본 데이터 소스 자리표시자: ScopeSentry 자산 동기화 MCP(주소와 인증은 비워 두고 비활성 상태).
-- 「자산 동기화」 페이지가 데이터 소스 설정 여부를 확인하는 데 사용한다. 사용자가 페이지에서 url과 X-API-Key를 입력한 뒤 활성화한다.
-- 없을 때만 삽입하며, 사용자가 이미 설정/활성화한 서버는 절대 덮어쓰지 않는다(schema.sql은 매 시작마다 Exec된다).
INSERT INTO mcp_servers (name, transport, url, env, enabled)
VALUES ('ScopeSentry', 'http', NULL, '{"X-API-Key":""}', false)
ON CONFLICT (name) DO NOTHING;

CREATE TABLE IF NOT EXISTS mcp_tools_cache (
    id            BIGSERIAL PRIMARY KEY,
    server_id     BIGINT NOT NULL REFERENCES mcp_servers(id) ON DELETE CASCADE,
    tool_name     TEXT NOT NULL,
    description   TEXT,
    schema        JSONB,
    discovered_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (server_id, tool_name)
);

-- =====================================================================
-- G. 가시성: agent × mcp / skill
-- =====================================================================
CREATE TABLE IF NOT EXISTS agent_visibility (
    agent_id      BIGINT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    resource_kind TEXT   NOT NULL CHECK (resource_kind IN ('mcp')),
    resource_id   BIGINT NOT NULL,
    mcp_tool_name TEXT   NOT NULL DEFAULT '',
    enabled       BOOLEAN NOT NULL DEFAULT true,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (agent_id, resource_kind, resource_id, mcp_tool_name)
);
CREATE INDEX IF NOT EXISTS idx_vis_resource ON agent_visibility(resource_kind, resource_id);

CREATE TABLE IF NOT EXISTS agent_skill_visibility (
    agent_id   BIGINT NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    skill_name TEXT   NOT NULL,
    enabled    BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (agent_id, skill_name)
);
CREATE INDEX IF NOT EXISTS idx_askv_skill ON agent_skill_visibility(skill_name);

-- Skill 호출 원장(db/skill_usage.go 참고). Skill() 호출 한 번당 한 행이며, 차원만 기록하고 본문은 기록하지 않는다.
-- 의도적으로 외래키를 두지 않는다: 태스크/세션 삭제 후에도 통계는 남아야 하며(llm_usage와 같은 이유), skill 자체도
-- 파일시스템상의 디렉터리 이름일 뿐이므로 대응하는 테이블이 없다.
CREATE TABLE IF NOT EXISTS skill_usage (
    id             BIGSERIAL PRIMARY KEY,
    ts             TIMESTAMPTZ NOT NULL DEFAULT now(),
    skill          TEXT NOT NULL,
    agent_key      TEXT,
    task_id        BIGINT,
    exploration_id BIGINT,
    intent_id      BIGINT,
    session_id     TEXT,
    args_len       INTEGER NOT NULL DEFAULT 0,
    -- false = 모델이 존재하지 않는 skill을 지목함(미적중). 이런 행도 보존한다: 이는 「원하지만 없음」이라는
    -- 공백을 나타내며, skill을 보충할 근거가 된다.
    found          BOOLEAN NOT NULL DEFAULT true
);
CREATE INDEX IF NOT EXISTS idx_skill_usage_skill ON skill_usage(skill, ts DESC);
CREATE INDEX IF NOT EXISTS idx_skill_usage_task  ON skill_usage(task_id);

-- 도구 호출 원장(db/tool_usage.go 참고). 실제 CoreTool.Call 한 번당 한 행이며, 귀속 차원만 기록하고
-- 도구 인자나 반환 내용은 저장하지 않는다. 의도적으로 외래키를 두지 않아 태스크, 세션 또는 커스텀 도구가 삭제되어도 통계는 남는다.
CREATE TABLE IF NOT EXISTS tool_usage (
    id             BIGSERIAL PRIMARY KEY,
    ts             TIMESTAMPTZ NOT NULL DEFAULT now(),
    tool_key       TEXT NOT NULL,
    agent_key      TEXT,
    task_id        BIGINT,
    exploration_id BIGINT,
    intent_id      BIGINT,
    session_id     TEXT
);
CREATE INDEX IF NOT EXISTS idx_tool_usage_tool ON tool_usage(tool_key, ts DESC);
CREATE INDEX IF NOT EXISTS idx_tool_usage_task ON tool_usage(task_id);

-- =====================================================================
-- H. 내장 도구 카탈로그
-- =====================================================================
CREATE TABLE IF NOT EXISTS tools (
    key         TEXT PRIMARY KEY,
    system      BOOLEAN NOT NULL DEFAULT true,
    description TEXT    NOT NULL DEFAULT '',
    schema      JSONB   NOT NULL DEFAULT '{}',
    agents      JSONB   NOT NULL DEFAULT '[]',
    enabled     BOOLEAN NOT NULL DEFAULT true,
    kind        TEXT    NOT NULL DEFAULT 'builtin',
    exec        JSONB   NOT NULL DEFAULT '{}',
    deferred    BOOLEAN NOT NULL DEFAULT false,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
DROP TRIGGER IF EXISTS trg_tools_upd ON tools;
CREATE TRIGGER trg_tools_upd BEFORE UPDATE ON tools
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- =====================================================================
-- I. 세션(대화 페이지)
-- =====================================================================
CREATE TABLE IF NOT EXISTS conversations (
    id             BIGSERIAL PRIMARY KEY,
    agent_key      TEXT NOT NULL,
    title          TEXT NOT NULL DEFAULT '',
    llm_profile_id BIGINT REFERENCES llm_profiles(id) ON DELETE SET NULL,
    pinned_at      TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE conversations ADD COLUMN IF NOT EXISTS pinned_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_conversations_llm_profile ON conversations(llm_profile_id) WHERE llm_profile_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_conversations_pinned ON conversations(pinned_at DESC) WHERE pinned_at IS NOT NULL;
DROP TRIGGER IF EXISTS trg_conversations_upd ON conversations;
CREATE TRIGGER trg_conversations_upd BEFORE UPDATE ON conversations
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS conversation_activities (
    id                 BIGSERIAL PRIMARY KEY,
    conversation_id    BIGINT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    worker             TEXT,
    kind               TEXT,
    tool               TEXT,
    tool_use_id        TEXT,
    is_error           BOOLEAN NOT NULL DEFAULT false,
    summary            TEXT,
    detail             TEXT,
    input_tokens       INTEGER,
    output_tokens      INTEGER,
    cache_read_tokens  INTEGER,
    cache_write_tokens INTEGER,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_conv_act ON conversation_activities(conversation_id, id);
CREATE INDEX IF NOT EXISTS idx_conv_act_tool_call ON conversation_activities(conversation_id, tool_use_id, id)
  WHERE kind IN ('tool_use', 'tool_result');

-- =====================================================================
-- J. Agent 트리거
-- =====================================================================
CREATE TABLE IF NOT EXISTS agent_triggers (
    id                          BIGSERIAL PRIMARY KEY,
    agent_key                   TEXT NOT NULL,
    enabled                     BOOLEAN NOT NULL DEFAULT true,
    interval_sec                INTEGER NOT NULL DEFAULT 0,
    on_finding                  BOOLEAN NOT NULL DEFAULT false,
    on_goal_met                 BOOLEAN NOT NULL DEFAULT false,
    on_task_timeout             BOOLEAN NOT NULL DEFAULT false,
    on_tool_call                BOOLEAN NOT NULL DEFAULT false,
    on_task_create              BOOLEAN NOT NULL DEFAULT false,
    interval_message            TEXT NOT NULL DEFAULT '',
    finding_message             TEXT NOT NULL DEFAULT '',
    goal_message                TEXT NOT NULL DEFAULT '',
    task_timeout_message        TEXT NOT NULL DEFAULT '',
    tool_call_message           TEXT NOT NULL DEFAULT '',
    task_create_message         TEXT NOT NULL DEFAULT '',
    tool_names                  TEXT NOT NULL DEFAULT '',
    last_fire                   TIMESTAMPTZ,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_agent_triggers_agent ON agent_triggers(agent_key);
-- 컬럼 추가 마이그레이션(이미 배포됨, 기존 DB 업그레이드 시 컬럼 보완; 새 DB의 CREATE에는 이 컬럼들이 이미 있으므로 ALTER는 no-op). 멱등하며 매 시작마다 반복 실행해도 안전하다.
ALTER TABLE agent_triggers ADD COLUMN IF NOT EXISTS on_tool_call        BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE agent_triggers ADD COLUMN IF NOT EXISTS tool_call_message   TEXT    NOT NULL DEFAULT '';
ALTER TABLE agent_triggers ADD COLUMN IF NOT EXISTS tool_names          TEXT    NOT NULL DEFAULT '';
ALTER TABLE agent_triggers ADD COLUMN IF NOT EXISTS on_task_create      BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE agent_triggers ADD COLUMN IF NOT EXISTS task_create_message TEXT    NOT NULL DEFAULT '';
DROP TRIGGER IF EXISTS trg_agent_triggers_upd ON agent_triggers;
CREATE TRIGGER trg_agent_triggers_upd BEFORE UPDATE ON agent_triggers
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS scheduler_state (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL DEFAULT ''
);

-- =====================================================================
-- K. 명령 차단 규칙
-- =====================================================================
CREATE TABLE IF NOT EXISTS intercept_rules (
    id              BIGSERIAL PRIMARY KEY,
    name            TEXT NOT NULL,
    enabled         BOOLEAN NOT NULL DEFAULT true,
    priority        INTEGER NOT NULL DEFAULT 0,
    match_target    TEXT NOT NULL CHECK (match_target IN ('tool_name', 'tool_input')),
    match_type      TEXT NOT NULL CHECK (match_type IN ('string', 'regex')),
    pattern         TEXT NOT NULL,
    action          TEXT NOT NULL CHECK (action IN ('allow', 'deny', 'ask')),
    message         TEXT NOT NULL DEFAULT '',
    timeout_enabled BOOLEAN NOT NULL DEFAULT true,
    timeout_seconds INTEGER NOT NULL DEFAULT 60,
    timeout_action  TEXT    NOT NULL DEFAULT 'deny',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
DROP TRIGGER IF EXISTS trg_intercept_rules_upd ON intercept_rules;
CREATE TRIGGER trg_intercept_rules_upd BEFORE UPDATE ON intercept_rules
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE IF NOT EXISTS intercept_pending (
    id              BIGSERIAL PRIMARY KEY,
    rule_id         BIGINT REFERENCES intercept_rules(id) ON DELETE SET NULL,
    conversation_id BIGINT REFERENCES conversations(id) ON DELETE CASCADE,
    task_id         TEXT,
    agent_name      TEXT NOT NULL DEFAULT '',
    tool_name       TEXT NOT NULL,
    tool_input      JSONB NOT NULL DEFAULT '{}',
    status          TEXT NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'allowed', 'denied', 'timeout')),
    -- 판정 사유: 규칙이 히트하면 규칙의 message; LLM 폴백 판정이면 모델이 준 짧은 사유(접두사 [模型]).
    reason          TEXT NOT NULL DEFAULT '',
    decided_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_intercept_pending_status ON intercept_pending(status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_intercept_pending_task   ON intercept_pending(task_id, created_at DESC);
-- 기존 DB 보완: reason 컬럼(이미 배포됨, 컬럼 추가 시 IF NOT EXISTS 필수).
ALTER TABLE intercept_pending ADD COLUMN IF NOT EXISTS reason TEXT NOT NULL DEFAULT '';
-- Detail payloads are lazy-loaded; NULL preserves the meaning of legacy history.
ALTER TABLE intercept_pending ADD COLUMN IF NOT EXISTS audit JSONB;
ALTER TABLE intercept_pending ADD COLUMN IF NOT EXISTS decision_source TEXT NOT NULL DEFAULT '';
UPDATE intercept_pending SET decision_source=CASE WHEN rule_id IS NOT NULL THEN 'rule'
 WHEN reason LIKE '[模型]%' THEN 'model' ELSE 'unknown' END WHERE decision_source='';

-- =====================================================================
-- L. 취약점 발견 영속화
-- =====================================================================
CREATE TABLE IF NOT EXISTS findings (
    id          BIGSERIAL PRIMARY KEY,
    task_id     BIGINT REFERENCES tasks(id) ON DELETE SET NULL,
    node_id     BIGINT REFERENCES exploration_nodes(id) ON DELETE SET NULL,
    vulnclass   TEXT NOT NULL DEFAULT '',
    -- 취약점 이름(읽기 쉬운 제목); 비어 있으면 프론트엔드가 vulnclass를 대신 표시한다. severity 값:
    -- critical 치명 / high 높음 / medium 중간 / low 낮음 (CHECK는 추가하지 않음. status와 마찬가지로 server 측 화이트리스트로 검증한다).
    name        TEXT NOT NULL DEFAULT '',
    severity    TEXT NOT NULL DEFAULT '',
    summary     TEXT NOT NULL DEFAULT '',
    evidence    TEXT NOT NULL DEFAULT '',
    worker      TEXT NOT NULL DEFAULT '',
    asset_ids   JSONB NOT NULL DEFAULT '[]',
    -- 처리 상태: pending 대기 / in_progress 처리 중 / confirmed 확인됨 / resolved 처리 완료 / fixed 수정됨 /
    -- false_positive 오탐 / ignored 무시 / duplicate 중복 / risk_accepted 위험 수용.
    -- 값에는 CHECK를 두지 않는다: 기존 DB는 아래 ALTER로 컬럼을 보완하며, CHECK는 백필할 수 없으므로 server 측 화이트리스트로 통일 검증한다.
    status      TEXT NOT NULL DEFAULT 'pending',
    -- 취약점 상세 보고서(Markdown); 기본값은 비어 있으며, 상세 페이지에서만 읽어 표시한다. 목록 API에는 포함하지 않아 payload가 커지지 않도록 한다.
    report      TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE findings ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'pending';
ALTER TABLE findings ADD COLUMN IF NOT EXISTS name   TEXT NOT NULL DEFAULT '';
ALTER TABLE findings ADD COLUMN IF NOT EXISTS report TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_findings_task ON findings(task_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_findings_time ON findings(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_findings_status ON findings(status, created_at DESC);
-- 「자산별」 뷰는 asset_ids @> '[<id>]'로 발견 항목을 역조회하므로, 이 GIN 인덱스가 없으면 전체 테이블 스캔이 된다.
CREATE INDEX IF NOT EXISTS idx_findings_asset_ids ON findings USING GIN(asset_ids jsonb_path_ops);

-- 수동 재테스트는 독립된 세션에 속하며, 결론과 원래 취약점의 처리 상태는 분리해 저장한다.
CREATE TABLE IF NOT EXISTS finding_retests (
    id BIGSERIAL PRIMARY KEY,
    finding_id BIGINT NOT NULL REFERENCES findings(id) ON DELETE CASCADE,
    conversation_id BIGINT UNIQUE REFERENCES conversations(id) ON DELETE SET NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','running','completed','failed','stopped')),
    verdict TEXT NOT NULL DEFAULT '' CHECK (verdict IN ('','reproduced','fixed','inconclusive')),
    notes TEXT NOT NULL DEFAULT '',
    snapshot JSONB NOT NULL,
    summary TEXT NOT NULL DEFAULT '',
    evidence TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    finished_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_finding_retests_history ON finding_retests(finding_id, id DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_finding_retests_active ON finding_retests(finding_id)
    WHERE status IN ('pending','running');

-- 세션 삭제 시 재테스트 기록은 보존하고, 아직 끝나지 않은 재테스트 점유도 함께 해제한다.
CREATE OR REPLACE FUNCTION stop_deleted_conversation_retest() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    UPDATE finding_retests SET status='stopped', error='复测会话已删除', finished_at=now()
    WHERE conversation_id=OLD.id AND status IN ('pending','running');
    RETURN OLD;
END;
$$;
DROP TRIGGER IF EXISTS trg_conversation_retest_delete ON conversations;
CREATE TRIGGER trg_conversation_retest_delete BEFORE DELETE ON conversations
    FOR EACH ROW EXECUTE FUNCTION stop_deleted_conversation_retest();

ALTER TABLE findings ADD COLUMN IF NOT EXISTS evidence_version BIGINT NOT NULL DEFAULT 0;
ALTER TABLE findings ADD COLUMN IF NOT EXISTS report_evidence_version BIGINT NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS traffic_evidence_snapshots (
    id TEXT PRIMARY KEY,
    source_traffic_id TEXT NOT NULL,
    captured_at BIGINT NOT NULL,
    url TEXT NOT NULL,
    method TEXT NOT NULL,
    status INTEGER NOT NULL,
    content_type TEXT NOT NULL DEFAULT '',
    req_head TEXT NOT NULL,
    resp_head TEXT NOT NULL,
    req_hash TEXT NOT NULL,
    resp_hash TEXT NOT NULL,
    req_len BIGINT NOT NULL,
    resp_len BIGINT NOT NULL,
    unreferenced_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS finding_traffic_bindings (
    id BIGSERIAL PRIMARY KEY,
    finding_id BIGINT NOT NULL REFERENCES findings(id) ON DELETE CASCADE,
    snapshot_id TEXT NOT NULL REFERENCES traffic_evidence_snapshots(id),
    role TEXT NOT NULL DEFAULT 'supporting',
    note TEXT NOT NULL DEFAULT '',
    position INTEGER NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(finding_id, snapshot_id)
);
CREATE INDEX IF NOT EXISTS idx_finding_traffic_order ON finding_traffic_bindings(finding_id, position, id);
CREATE INDEX IF NOT EXISTS idx_finding_traffic_snapshot ON finding_traffic_bindings(snapshot_id);

-- =====================================================================
-- M. 백엔드 로그 영속화
-- =====================================================================
CREATE TABLE IF NOT EXISTS server_logs (
    id         BIGSERIAL PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    level      TEXT NOT NULL DEFAULT 'info',
    tag        TEXT NOT NULL DEFAULT '',
    text       TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_server_logs_id ON server_logs(id DESC);

-- Independent /btw history and the latest provider-ready main checkpoint.
CREATE TABLE IF NOT EXISTS side_question_sessions (
    session_key TEXT PRIMARY KEY,
    conversation_id BIGINT REFERENCES conversations(id) ON DELETE CASCADE,
    task_id BIGINT REFERENCES tasks(id) ON DELETE CASCADE,
    exploration_id BIGINT REFERENCES explorations(id) ON DELETE CASCADE,
    intent_id BIGINT REFERENCES exploration_nodes(id) ON DELETE CASCADE,
    run_id BIGINT NOT NULL,
    version BIGINT NOT NULL,
    snapshot JSONB NOT NULL,
    generation BIGINT NOT NULL DEFAULT 0,
    CHECK ((conversation_id IS NOT NULL AND task_id IS NULL AND exploration_id IS NULL AND intent_id IS NULL)
        OR (conversation_id IS NULL AND task_id IS NOT NULL AND exploration_id IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS idx_side_sessions_conv ON side_question_sessions(conversation_id);
CREATE INDEX IF NOT EXISTS idx_side_sessions_task ON side_question_sessions(task_id);
CREATE INDEX IF NOT EXISTS idx_side_sessions_exp ON side_question_sessions(exploration_id);
CREATE INDEX IF NOT EXISTS idx_side_sessions_intent ON side_question_sessions(intent_id);

CREATE TABLE IF NOT EXISTS side_question_requests (
    id TEXT PRIMARY KEY,
    ordinal BIGSERIAL UNIQUE,
    session_key TEXT NOT NULL REFERENCES side_question_sessions(session_key) ON DELETE CASCADE,
    generation BIGINT NOT NULL,
    client_id TEXT NOT NULL,
    question TEXT NOT NULL,
    answer TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK(status IN ('running','completed','failed','cancelled','interrupted')),
    error TEXT NOT NULL DEFAULT '',
    model JSONB NOT NULL,
    snapshot_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    sequence BIGINT NOT NULL DEFAULT 0,
    usage JSONB NOT NULL DEFAULT '{}',
    UNIQUE(session_key,generation,client_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_side_request_running ON side_question_requests(session_key) WHERE status='running';
CREATE INDEX IF NOT EXISTS idx_side_requests_history ON side_question_requests(session_key,ordinal DESC);

-- Additive v3 archive fields; old archives restore these as empty objects.
ALTER TABLE side_question_sessions ADD COLUMN IF NOT EXISTS memory JSONB NOT NULL DEFAULT '{}';
ALTER TABLE side_question_requests ADD COLUMN IF NOT EXISTS context_info JSONB NOT NULL DEFAULT '{}';

-- =====================================================================
-- 자산 차단 규칙 (전역 블랙리스트)
-- §K 명령 차단(intercept_rules)과 독립적이다: intercept_rules는 도구 이름/입력 인자 텍스트를 매칭하고,
-- 이 테이블은 「대상 자산」을 매칭한다 — 완전 일치/퍼지 도메인·IP·URL 및 CIDR 네트워크 대역.
-- 규칙만 저장한다. 구체적인 매칭/차단 로직은 다른 곳에서 구현한다.
-- kind 일곱 가지:
--   exact_domain / exact_ip / exact_url  —— 완전 일치 매칭
--   fuzzy_domain / fuzzy_ip / fuzzy_url  —— 퍼지 매칭
--   cidr                                 —— CIDR 네트워크 대역
-- =====================================================================
CREATE TABLE IF NOT EXISTS asset_intercept_rules (
    id          BIGSERIAL PRIMARY KEY,
    enabled     BOOLEAN NOT NULL DEFAULT true,
    kind        TEXT NOT NULL CHECK (kind IN (
                    'exact_domain', 'exact_ip', 'exact_url',
                    'fuzzy_domain', 'fuzzy_ip', 'fuzzy_url',
                    'cidr')),
    pattern     TEXT NOT NULL,
    note        TEXT NOT NULL DEFAULT '',
    builtin     BOOLEAN NOT NULL DEFAULT false,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_asset_intercept_enabled ON asset_intercept_rules(enabled);
DROP TRIGGER IF EXISTS trg_asset_intercept_rules_upd ON asset_intercept_rules;
CREATE TRIGGER trg_asset_intercept_rules_upd BEFORE UPDATE ON asset_intercept_rules
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- =====================================================================
-- 태스크 레벨 자산 차단/허용 규칙
-- 전역 asset_intercept_rules와 동형(kind/pattern/note/enabled)이지만 task_id로 연결되어
-- 태스크 삭제 시 함께 삭제된다. 태스크 생성 시 입력하고 태스크 상세에서 편집한다.
-- action: 'block'=차단(테스트 금지)  'allow'=허용(화이트리스트)
-- 실행 판정: 먼저 차단 규칙(전역 ∪ 태스크 block)으로 매칭하며, 히트하면 금지한다. 히트하지 않고 해당 태스크에 활성 allow 규칙이 있으면
-- 어떤 allow 규칙에 맞아야 통과하고, 그렇지 않으면 「테스트 불허」가 된다.
-- =====================================================================
CREATE TABLE IF NOT EXISTS task_intercept_rules (
    id          BIGSERIAL PRIMARY KEY,
    task_id     BIGINT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    enabled     BOOLEAN NOT NULL DEFAULT true,
    action      TEXT NOT NULL DEFAULT 'block' CHECK (action IN ('block','allow')),
    kind        TEXT NOT NULL CHECK (kind IN (
                    'exact_domain', 'exact_ip', 'exact_url',
                    'fuzzy_domain', 'fuzzy_ip', 'fuzzy_url',
                    'cidr')),
    pattern     TEXT NOT NULL,
    note        TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_task_intercept_task ON task_intercept_rules(task_id);
-- 기존 DB 보완(이 세션 앞부분에서 이미 만든 테이블에 action 컬럼이 없음): 컬럼 추가(IF NOT EXISTS 포함).
ALTER TABLE task_intercept_rules ADD COLUMN IF NOT EXISTS action TEXT NOT NULL DEFAULT 'block';
DROP TRIGGER IF EXISTS trg_task_intercept_rules_upd ON task_intercept_rules;
CREATE TRIGGER trg_task_intercept_rules_upd BEFORE UPDATE ON task_intercept_rules
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- =====================================================================
-- M. 취약점 IM 푸시
--
-- 세 테이블을 일부러 분리했으며, 핵심은 **폭발 반경(blast radius)**이다: 취약점을 기록하는 트랜잭션(RecordFindingTx,
-- 태스크 행 잠금을 보유)은 블라인드 INSERT 한 번만 허용되며, 채널 테이블을 읽지도 않고 사용자의 필터 규칙을 실행하지도 않는다. 그렇지 않으면
-- 잘못 설정된 webhook 필터 조건 하나가 트랜잭션을 오염시키거나 중단시켜 취약점이 저장되지 못할 수 있다.
--
--   notification_channels   채널 인스턴스 설정(변경 가능, 자격 증명 포함, UI에서 관리)
--   notification_events     이벤트 사실(취약점 기록 트랜잭션 안에서 블라인드 삽입, 렌더링 스냅샷 포함)
--   notification_deliveries 전송 태스크(트랜잭션 밖 fan-out으로 생성, 상태/재시도/배치 보유)
-- =====================================================================

-- 채널 인스턴스: 같은 kind를 여러 개 설정할 수 있다(예: 「긴급 그룹」「일상 그룹」 각각 DingTalk 봇 하나씩).
-- kind 값은 server 측 화이트리스트로 검증하며 CHECK는 두지 않는다: findings.status와 같은 이유이며,
-- 나중에 채널을 추가해도 테이블 구조를 바꿀 필요가 없어야 한다.
CREATE TABLE IF NOT EXISTS notification_channels (
    id           BIGSERIAL PRIMARY KEY,
    name         TEXT NOT NULL,
    -- dingtalk(딩톡) / feishu(페이슈) / wecom(기업 위챗) / webhook 범용 / telegram / email
    kind         TEXT NOT NULL,
    enabled      BOOLEAN NOT NULL DEFAULT true,
    -- 자격 증명(평문 저장, UI에서는 마스킹 표시; server 측 maskChannelSecrets 참고). 여섯 가지 채널의 필드 차이가 매우 커서,
    -- NULL 컬럼을 채널마다 늘리지 않도록 JSONB로 통일하고 Go 측에서 kind별로 엄격히 검증한다:
    --   dingtalk {webhook,secret}
    --   feishu   {webhook,secret}
    --   wecom    {webhook}
    --   webhook  {url,method,content_type,headers{},body_template}
    --   telegram {bot_token,chat_id,base_url}
    --   email    {host,port,username,password,from,to[],tls}
    config       JSONB NOT NULL DEFAULT '{}',
    -- 푸시 시점: realtime은 일치 즉시 푸시 / digest는 배치에 넣었다가 전역 주기마다 하나의 메시지로 요약한다.
    mode         TEXT NOT NULL DEFAULT 'realtime',
    -- 필터 조건, 모든 필드는 선택 사항(기본값 = 필터링 안 함):
    --   min_severity       ''|low|medium|high|critical
    --   task_ids/asset_ids 빈 배열 = 제한 없음; 비어 있지 않으면 교집합이 있어야 함
    --   vulnclass_include/exclude 키워드 배열(대소문자 무시 부분 문자열); include가 비어 있으면 모두 수집
    --   on_status_change bool, realtime 모드에서만 의미가 있음
    filter       JSONB NOT NULL DEFAULT '{}',
    -- 분당 전송 상한; 0 = 제한 없음. 기본값 20은 DingTalk/WeCom 공식 하드 제한에 맞춘 값이다.
    -- 상한 초과 시 메시지를 버리지 않고 전송만 다음 tick으로 미룬다.
    rate_per_min INTEGER NOT NULL DEFAULT 20,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
DROP TRIGGER IF EXISTS trg_notification_channels_upd ON notification_channels;
CREATE TRIGGER trg_notification_channels_upd BEFORE UPDATE ON notification_channels
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- 이벤트 사실. RecordFindingTx / 상태 변경 트랜잭션에서 **같은 트랜잭션**으로 기록하여 「취약점 저장」과 「푸시 태스크 존재」가 원자적으로 일치하도록 보장한다.
-- 커밋은 성공했는데 큐에 들어가지 않아 메시지가 영구 유실되는 구간이 없다.
-- snapshot은 의도적으로 중복 저장한다: 취약점은 나중에 이름/등급/상태가 바뀔 수 있으므로, 푸시 내용은 「사건 발생 당시」를 반영해야 하며,
-- fan-out과 렌더링이 findings/tasks/assets 여러 테이블을 다시 조회하지 않아도 되게 하기 위함이다.
-- finding 삭제 시 이벤트는 연쇄 삭제되지 않는다: findings 테이블의 「태스크 삭제 후에도 독립적으로 보존」 의미와 일치시키기 위함이다.
CREATE TABLE IF NOT EXISTS notification_events (
    id         BIGSERIAL PRIMARY KEY,
    -- finding_created | finding_status_changed
    kind       TEXT NOT NULL,
    finding_id BIGINT NOT NULL,
    snapshot   JSONB NOT NULL,
    -- fan-out 멱등 표시: dispatcher는 이 컬럼으로 대기 중인 이벤트를 가져오며, 처리가 끝나면 true로 바꾼다.
    -- 행을 삭제하지 않고 컬럼으로 표시하는 이유는 전송 이력이 이벤트까지 거슬러 올라갈 수 있게 하기 위함이다.
    fanned_out BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_notification_events_pending
    ON notification_events(id) WHERE NOT fanned_out;

-- 전송 태스크: 이벤트 1건 × 활성 채널 1개 = 1행. fan-out은 트랜잭션 밖에서 수행하므로, 채널을 나중에 켜도
-- 과거 이력은 보충되지 않는다(agent_triggers의 「늦게 켠 trigger는 과거 이력을 보충하지 않음」과 같은 의미). 채널을 켤 때
-- 과거 적체 이력이 한꺼번에 쏟아지는 것을 막기 위함이다.
-- channel_id는 연쇄 삭제된다: 채널 설정이 없어지면 그 전송 이력도 의미가 없다.
CREATE TABLE IF NOT EXISTS notification_deliveries (
    id          BIGSERIAL PRIMARY KEY,
    event_id    BIGINT NOT NULL REFERENCES notification_events(id) ON DELETE CASCADE,
    channel_id  BIGINT NOT NULL REFERENCES notification_channels(id) ON DELETE CASCADE,
    -- pending 대기 / sent 전송됨 / failed 재시도 소진(수동 재전송 가능) / skipped 채널 비활성화 또는 배치 취소
    -- pending 대기 / sending 어떤 dispatcher가 이미 가져감(임대 미만료) / sent 전송됨 /
    -- failed 재시도 소진 또는 영구 실패(수동 재전송 가능) / skipped 채널 비활성화. 값에는 CHECK를 두지 않으며,
    -- findings.status와 같은 이유로 server 측 화이트리스트에서 검증한다.
    state       TEXT NOT NULL DEFAULT 'pending',
    attempts    INTEGER NOT NULL DEFAULT 0,
    -- 「다음에 가져갈 수 있는 시각」과 「임대 만료 시각」을 겸한다: 가져갈 때 이 값을 미래로 밀면 그것이 곧 임대가 된다.
    -- 따라서 「임대 미만료」와 「재시도 시각 미도래」를 같은 조건식으로 표현할 수 있어 별도의 lease_until 컬럼이 필요 없다.
    -- 프로세스가 죽으며 남긴 sending 행은 임대가 만료되면 다음 라운드에 다시 가져간다.
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error  TEXT NOT NULL DEFAULT '',
    -- digest 모드는 같은 배치를 공유하며, realtime은 항상 NULL이다. 배치 전체를 한 메시지로 렌더링한 뒤 한꺼번에 sent로 바꾼다.
    batch_id    BIGINT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_notification_deliveries_due
    ON notification_deliveries(next_attempt_at) WHERE state='pending';
CREATE INDEX IF NOT EXISTS idx_notification_deliveries_history
    ON notification_deliveries(id DESC);
CREATE INDEX IF NOT EXISTS idx_notification_deliveries_batch
    ON notification_deliveries(batch_id) WHERE batch_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_notification_deliveries_channel
    ON notification_deliveries(channel_id, id DESC);
