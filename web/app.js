const app = document.getElementById("app");
const healthDot = document.getElementById("health-dot");
const healthText = document.getElementById("health-text");

const STATUS_CN = {
  pending: "排队", running: "执行中", succeeded: "成功", failed: "失败", cancelled: "已取消",
  passed: "通过", warning: "警告", fetch_logs: "拉取日志", parse: "解析", done: "完成",
  catalog: "目录", wal_check: "WAL 校验", scan: "扫描", restore: "恢复", unload: "导出", dropscan: "碎页扫描",
  pdu: "PDU", online: "在线",
};

let pollTimer = 0;
let sqlPage = 1;
let wizard = { step: 1, precheck: null, form: null };

function stopPoll() {
  if (pollTimer) { clearInterval(pollTimer); pollTimer = 0; }
}

async function api(path, opts) {
  const res = await fetch(path, {
    headers: { "Content-Type": "application/json", ...(opts && opts.headers) },
    ...opts,
  });
  let data = {};
  try { data = await res.json(); } catch (_) { throw new Error("接口返回不是 JSON"); }
  if (!res.ok || (data.code && data.code !== "0")) {
    throw new Error((data.details && data.details.err) || data.message || "请求失败");
  }
  return data.result;
}

function esc(s) {
  return String(s == null ? "" : s)
    .replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
}

function fmtTime(v) {
  if (!v) return "—";
  const d = new Date(v);
  if (Number.isNaN(d.getTime())) return String(v);
  const p = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
}

function badge(status) {
  const key = String(status || "").toLowerCase();
  return `<span class="badge ${esc(key)}">${esc(STATUS_CN[key] || status || "未知")}</span>`;
}

function route() {
  const raw = location.hash.replace(/^#/, "") || "/";
  const [path, qs] = raw.split("?");
  const q = new URLSearchParams(qs || "");
  const task = path.match(/^\/tasks\/([^/]+)/);
  if (task) return { name: "task", id: decodeURIComponent(task[1]), q };
  if (path === "/history") return { name: "history", q };
  if (path === "/instances") return { name: "instances", q };
  if (path === "/ops" || path === "/cloud") return { name: "ops", q };
  if (path === "/tools" || path === "/selftest") return { name: "tools", q };
  if (path === "/new" || path === "/") return { name: "new", q };
  return { name: "new", q };
}

function setNav(name) {
  const map = { task: "history", cloud: "ops", selftest: "tools", list: "history" };
  const key = map[name] || name;
  document.querySelectorAll("#nav a").forEach((a) => a.classList.toggle("active", a.dataset.route === key));
}

async function pingHealth() {
  try {
    const res = await fetch("/healthz");
    const data = await res.json();
    const ok = data && data.status === "ok";
    healthDot.className = ok ? "ok" : "bad";
    healthText.textContent = ok ? "就绪" : "异常";
  } catch (_) {
    healthDot.className = "bad";
    healthText.textContent = "离线";
  }
}

async function refreshRecent() {
  const box = document.getElementById("recent-tasks");
  if (!box) return;
  try {
    const data = await api("/api/v1/flashback/tasks?page=1&page_size=5");
    const list = (data && data.list) || [];
    box.innerHTML = list.length
      ? list.map((t) => `<a href="#/tasks/${encodeURIComponent(t.id)}"><span class="dot ${esc(t.status)}"></span>${esc(t.database || t.instance_id)} ${esc(STATUS_CN[t.status] || t.status)}</a>`).join("")
      : `<div class="meta">暂无任务</div>`;
  } catch (_) {
    box.innerHTML = `<div class="meta">暂无</div>`;
  }
}

const HISTORY_PAGE_SIZE = 10;

function historyPageItems(page, pages) {
  const items = [];
  const push = (p) => items.push({ p, label: String(p) });
  if (pages <= 7) {
    for (let i = 1; i <= pages; i++) push(i);
    return items;
  }
  push(1);
  const from = Math.max(2, page - 1);
  const to = Math.min(pages - 1, page + 1);
  if (from > 2) items.push({ dots: true });
  for (let i = from; i <= to; i++) push(i);
  if (to < pages - 1) items.push({ dots: true });
  push(pages);
  return items;
}

function parseTables(raw) {
  return String(raw || "").split(/[\n,]/).map((s) => s.trim()).filter(Boolean);
}

function num(v) {
  const n = Number(v);
  return Number.isFinite(n) && n > 0 ? n : 0;
}

function toAPITime(v) {
  const s = String(v || "").trim();
  if (!s) return "";
  if (/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/.test(s)) return s.replace("T", " ") + ":00";
  return s;
}

function toLocalInput(d) {
  const p = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`;
}

async function loadInstances() {
  const list = await api("/api/v1/flashback/instances");
  return Array.isArray(list) ? list : [];
}

function instanceOptions(instances, selected) {
  if (!instances.length) return `<option value="">请先在「实例地址」添加</option>`;
  return instances.map((it) => {
    const label = `${it.id} · ${it.host}:${it.port}`;
    return `<option value="${esc(it.id)}" ${it.id === selected ? "selected" : ""}>${esc(label)}</option>`;
  }).join("");
}

function collectTaskForm(form) {
  const fd = new FormData(form);
  const sqlTypes = [...form.querySelectorAll("input[name=sql_type]:checked")].map((el) => el.value);
  const engine = String(fd.get("engine") || "native");
  const body = {
    engine,
    instance_id: String(fd.get("instance_id") || "").trim(),
    database: String(fd.get("database") || "").trim(),
    tables: parseTables(fd.get("tables")),
    target_time: toAPITime(fd.get("target_time")),
    end_time: toAPITime(fd.get("end_time")),
    output_kind: String(fd.get("output_kind") || "flashback"),
    sql_type: sqlTypes.join(","),
    start_xid: num(fd.get("start_xid")),
    stop_xid: num(fd.get("stop_xid")),
    start_file: String(fd.get("start_file") || "").trim(),
    start_pos: num(fd.get("start_pos")),
    stop_file: String(fd.get("stop_file") || "").trim(),
    stop_pos: num(fd.get("stop_pos")),
    dict_task_id: String(fd.get("dict_task_id") || "").trim(),
    cloud_instance_id: String(fd.get("cloud_instance_id") || "").trim(),
    cloud_region: String(fd.get("cloud_region") || "").trim(),
    pdu_scene: String(fd.get("pdu_scene") || "wal_delete"),
    pgdata_path: String(fd.get("pgdata_path") || "").trim(),
    archive_dest: String(fd.get("archive_dest") || "").trim(),
    disk_path: String(fd.get("disk_path") || "").trim(),
    pgdata_exclude: String(fd.get("pgdata_exclude") || "").trim(),
    start_wal: String(fd.get("start_wal") || "").trim(),
    end_wal: String(fd.get("end_wal") || "").trim(),
    export_mode: String(fd.get("export_mode") || "sql"),
  };
  Object.keys(body).forEach((k) => {
    if (body[k] === "" || body[k] === 0 || (Array.isArray(body[k]) && body[k].length === 0)) delete body[k];
  });
  if (engine !== "pdu") {
    delete body.pdu_scene;
    delete body.pgdata_path;
    delete body.archive_dest;
    delete body.disk_path;
    delete body.pgdata_exclude;
    delete body.start_wal;
    delete body.end_wal;
    delete body.export_mode;
    delete body.engine;
  }
  return body;
}

function stepsHTML(cur, pdu) {
  return `<div class="steps">
    <div class="step ${cur >= 1 ? "on" : ""}"><b>1</b> ${pdu ? "配置来源" : "配置连接"}</div>
    <div class="step ${cur >= 2 ? "on" : ""}"><b>2</b> 预检查</div>
    <div class="step ${cur >= 3 ? "on" : ""}"><b>3</b> 执行确认</div>
  </div>`;
}

const PDU_SCENE_CN = {
  wal_delete: "WAL 删除",
  wal_update: "WAL 更新前值",
  unload: "离线导出",
  drop_table: "DROP TABLE",
};

async function renderNew() {
  setNav("new");
  let instances = [];
  try { instances = await loadInstances(); } catch (err) {
    app.innerHTML = `<section class="card"><div class="err">${esc(err.message)}</div></section>`;
    return;
  }
  const now = new Date();
  const endLocal = toLocalInput(now);
  const startLocal = toLocalInput(new Date(now.getTime() - 10 * 60 * 1000));
  const prev = wizard.form || {};
  const engine = prev.engine === "pdu" ? "pdu" : "native";
  const scene = prev.pdu_scene || "wal_delete";
  const picked = instances[0];
  app.innerHTML = `<section class="card">
    <h2>闪回任务</h2>
    <p class="lead" id="mode-lead">${engine === "pdu" ? "只读本机 PGDATA / WAL。目标表：空=整库，一行=单表，多行=多表。时间窗须覆盖实际 DML 的 COMMIT 时间。" : "连接地址从「实例地址」获取，这里只选实例并设定时间窗。"}</p>
    ${stepsHTML(1, engine === "pdu")}
    <form id="task-form">
      <div class="field"><label>模式</label>
        <div class="chips" id="engine-chips">
          <button type="button" class="chip ${engine === "native" ? "on" : ""}" data-engine="native">在线闪回</button>
          <button type="button" class="chip ${engine === "pdu" ? "on" : ""}" data-engine="pdu">PDU 离线</button>
        </div>
      </div>
      <input type="hidden" name="engine" value="${engine}">
      <div id="pdu-scenes" class="scene-row" style="${engine === "pdu" ? "" : "display:none"}">
        ${["wal_delete","wal_update","unload","drop_table"].map((s) => `<button type="button" class="scene ${s === scene ? "on" : ""}" data-scene="${s}">${PDU_SCENE_CN[s]}</button>`).join("")}
      </div>
      <input type="hidden" name="pdu_scene" value="${esc(scene)}">
      <div class="split">
        <div>
          <h3 id="left-title">${engine === "pdu" ? "本机来源" : "连接配置"}</h3>
          <div class="field"><label id="inst-label">${engine === "pdu" ? "实例（可选）" : "已配置实例 *"}</label>
            <select name="instance_id">${engine === "pdu" ? `<option value="">不选 / 纯离线</option>` : ""}${instanceOptions(instances, prev.instance_id)}</select>
          </div>
          <div id="addr-box" class="addr" style="margin:12px 0">${picked ? "" : "还没有实例地址。请先到左侧「实例地址」添加。"}</div>
          <div id="pdu-paths" style="${engine === "pdu" ? "" : "display:none"}">
            <div class="field"><label>数据目录（实例）*</label><input name="pgdata_path" class="mono" placeholder="SHOW data_directory" value="${esc(prev.pgdata_path || "")}"></div>
            <div class="field" id="fld-archive"><label>WAL 目录（实例）</label><input name="archive_dest" class="mono" placeholder="pg_wal / archive_dest" value="${esc(prev.archive_dest || "")}"></div>
            <div class="field" id="fld-disk"><label>扫描路径 disk_path</label><input name="disk_path" class="mono" placeholder="/data/image" value="${esc(prev.disk_path || "")}"></div>
            <div class="field" id="fld-exclude"><label>排除目录 pgdata_exclude</label><input name="pgdata_exclude" class="mono" value="${esc(prev.pgdata_exclude || "")}"></div>
            <div class="actions" style="margin-top:0"><button type="button" class="btn ghost small" id="btn-discover">探测目录</button></div>
          </div>
          <div class="field"><label>数据库名 *</label><input name="database" required placeholder="业务库名" value="${esc(prev.database || "")}"></div>
          <div class="field"><label>目标表（空=整库；一行=单表；多行=多表；可只填 schema）</label><textarea name="tables" placeholder="public.t1&#10;public.t2">${esc((prev.tables || []).join("\n"))}</textarea></div>
          <div class="info tip" id="replica-tip">
            <strong>建议：误操作前对目标表设置 REPLICA IDENTITY FULL</strong>
            <p>默认 DEFAULT 时，WAL 里 DELETE/UPDATE 通常只记主键。闪回会尽量从堆页补齐其它列；行已被 VACUUM 清掉后，非主键列无法从 WAL 还原。</p>
            <pre class="mono">ALTER TABLE schema.table REPLICA IDENTITY FULL;</pre>
            <p>FULL 会把整行旧值写入 WAL，删除后也能还原。副作用是 UPDATE/DELETE 的 WAL 变大。无主键，或 IDENTITY 为 NOTHING 的表，更应提前改成 FULL。预检查会核对此项。</p>
          </div>
        </div>
        <div>
          <h3>闪回目标</h3>
          <div class="field"><label>起始时间 *（须早于误操作 COMMIT）</label><input name="target_time" type="datetime-local" value="${prev.target_time ? prev.target_time.replace(" ", "T").slice(0,16) : startLocal}" required></div>
          <div class="chips" id="rel-chips">
            <button type="button" class="chip" data-m="5">5m</button>
            <button type="button" class="chip on" data-m="10">10m</button>
            <button type="button" class="chip" data-m="60">1h</button>
            <button type="button" class="chip" data-m="360">6h</button>
          </div>
          <div class="field" style="margin-top:12px"><label>结束时间（须晚于误操作 COMMIT）</label><input name="end_time" type="datetime-local" value="${endLocal}"></div>
          <div class="field" id="fld-output"><label>输出</label>
            <select name="output_kind">
              <option value="flashback">Undo SQL</option>
              <option value="original">原始 Redo SQL</option>
            </select>
          </div>
          <div class="field" id="fld-export" style="${engine === "pdu" ? "" : "display:none"}"><label>导出格式</label>
            <select name="export_mode">
              <option value="sql" ${prev.export_mode === "sql" ? "selected" : ""}>SQL</option>
              <option value="csv" ${prev.export_mode === "csv" ? "selected" : ""}>CSV</option>
              <option value="both" ${prev.export_mode === "both" ? "selected" : ""}>SQL + CSV</option>
            </select>
          </div>
          <div class="field" id="fld-sqltype"><label>SQL 类型</label>
            <div class="chips">
              <label class="chip"><input type="checkbox" name="sql_type" value="insert"> insert</label>
              <label class="chip"><input type="checkbox" name="sql_type" value="update"> update</label>
              <label class="chip"><input type="checkbox" name="sql_type" value="delete"> delete</label>
              <label class="chip"><input type="checkbox" name="sql_type" value="ddl"> ddl</label>
            </div>
          </div>
          <input type="hidden" name="cloud_instance_id">
          <input type="hidden" name="cloud_region">
          <details class="adv" id="adv-online"><summary>高级位点</summary>
            <div class="grid">
              <div class="field"><label>start_file</label><input name="start_file"></div>
              <div class="field"><label>start_pos</label><input name="start_pos" type="number" min="0"></div>
              <div class="field"><label>stop_file</label><input name="stop_file"></div>
              <div class="field"><label>stop_pos</label><input name="stop_pos" type="number" min="0"></div>
            </div>
          </details>
          <details class="adv" id="adv-pdu" style="${engine === "pdu" ? "" : "display:none"}"><summary>高级：XID / WAL 文件</summary>
            <div class="grid">
              <div class="field"><label>start_xid</label><input name="start_xid" type="number" min="0"></div>
              <div class="field"><label>stop_xid</label><input name="stop_xid" type="number" min="0"></div>
              <div class="field"><label>start_wal</label><input name="start_wal" class="mono" value="${esc(prev.start_wal || "")}"></div>
              <div class="field"><label>end_wal</label><input name="end_wal" class="mono" value="${esc(prev.end_wal || "")}"></div>
            </div>
          </details>
        </div>
      </div>
    </form>
    <div class="actions">
      <button class="btn primary" id="btn-precheck" ${instances.length ? "" : "disabled"}>下一步：预检查</button>
    </div>
    <div id="form-msg"></div>
  </section>`;

  const form = document.getElementById("task-form");
  const isMySQLInst = () => {
    const it = instances.find((x) => x.id === form.instance_id.value);
    return !!(it && String(it.db_type || "").toLowerCase().includes("mysql"));
  };
  const syncReplicaTip = () => {
    const tip = document.getElementById("replica-tip");
    if (!tip) return;
    tip.style.display = (form.engine.value !== "pdu" && isMySQLInst()) ? "none" : "";
  };
  const applyInst = () => {
    const it = instances.find((x) => x.id === form.instance_id.value);
    const box = document.getElementById("addr-box");
    if (!it) {
      box.textContent = form.engine.value === "pdu" ? "可不选实例，手动填本机路径；选实例后会自动探测目录。" : "请选择已配置的实例地址";
      syncReplicaTip();
      return;
    }
    box.innerHTML = `<strong>${esc(it.id)}</strong> · ${esc(it.db_type || "")} · ${esc(it.host)}:${esc(it.port)} · 用户 ${esc(it.user || "—")}
      <div class="meta">来源 ${esc(it.source === "db" ? "地址库" : "YAML")} · vendor=${esc(it.vendor || "自建")} · ${esc(it.cloud_instance_id || "")} ${esc(it.region || "")}</div>`;
    form.cloud_instance_id.value = it.cloud_instance_id || "";
    form.cloud_region.value = it.region || "";
    syncReplicaTip();
  };
  const runDiscover = async ({ fillPaths, resetPaths } = {}) => {
    const msg = document.getElementById("form-msg");
    const discBtn = document.getElementById("btn-discover");
    if (discBtn) { discBtn.disabled = true; discBtn.textContent = "探测中…"; }
    msg.textContent = "";
    try {
      const result = await api("/api/v1/flashback/pdu/discover", {
        method: "POST",
        body: JSON.stringify({
          pgdata_path: resetPaths ? "" : form.pgdata_path.value.trim(),
          database: form.database.value.trim(),
          instance_id: form.instance_id.value.trim(),
        }),
      });
      if (!result.ok && !result.pgdata_path) {
        msg.innerHTML = `<div class="err">${esc(result.message || "探测失败")}</div>`;
        return;
      }
      if (fillPaths) {
        const pg = result.remote_pgdata || result.pgdata_path;
        const wal = result.remote_wal || result.archive_dest;
        if (pg) {
          form.pgdata_path.value = pg;
          form.pgdata_path.placeholder = pg;
        }
        if (wal) {
          form.archive_dest.value = wal;
          form.archive_dest.placeholder = wal;
        }
        if (form.pdu_scene.value === "drop_table" && result.pgdata_path && !form.disk_path.value.trim()) {
          form.disk_path.value = result.pgdata_path;
        }
      }
      if (!form.database.value && result.databases && result.databases[0]) form.database.value = result.databases[0].name;
      const names = (result.databases || []).map((d) => d.name).join(", ");
      const hint = result.message || (result.pgdata_path ? `PGDATA ${result.pgdata_path} · WAL ${result.archive_dest || ""}` : "");
      const cls = result.ok ? "ok" : "err";
      msg.innerHTML = `<div class="${cls}">${esc(result.pg_version ? "PG " + result.pg_version + " · " : "")}${esc(hint)}${names ? " · 库 " + esc(names) : ""}</div>`;
    } catch (err) {
      msg.innerHTML = `<div class="err">${esc(err.message)}</div>`;
    } finally {
      if (discBtn) { discBtn.disabled = false; discBtn.textContent = "探测目录"; }
    }
  };
  form.instance_id.onchange = () => {
    applyInst();
    if (form.engine.value === "pdu" && form.instance_id.value) runDiscover({ fillPaths: true, resetPaths: true });
  };
  applyInst();
  const syncMode = () => {
    const pdu = form.engine.value === "pdu";
    document.getElementById("mode-lead").textContent = pdu
      ? "只读本机 PGDATA / WAL。目标表：空=整库，一行=单表，多行=多表。时间窗须覆盖实际 DML 的 COMMIT 时间。"
      : "连接地址从「实例地址」获取，这里只选实例并设定时间窗。";
    document.getElementById("left-title").textContent = pdu ? "本机来源" : "连接配置";
    document.getElementById("inst-label").textContent = pdu ? "实例（可选）" : "已配置实例 *";
    document.getElementById("pdu-scenes").style.display = pdu ? "" : "none";
    document.getElementById("pdu-paths").style.display = pdu ? "" : "none";
    document.getElementById("fld-export").style.display = pdu ? "" : "none";
    document.getElementById("adv-online").style.display = pdu ? "none" : "";
    document.getElementById("adv-pdu").style.display = pdu ? "" : "none";
    const scene = form.pdu_scene.value;
    const needWal = scene === "wal_delete" || scene === "wal_update" || scene === "unload";
    document.getElementById("fld-archive").style.display = needWal ? "" : "none";
    document.getElementById("fld-disk").style.display = scene === "drop_table" ? "" : "none";
    document.getElementById("fld-exclude").style.display = scene === "drop_table" ? "" : "none";
    document.getElementById("btn-precheck").disabled = pdu ? false : !instances.length;
    syncReplicaTip();
  };
  document.getElementById("engine-chips").onclick = (ev) => {
    const btn = ev.target.closest("[data-engine]");
    if (!btn) return;
    form.engine.value = btn.dataset.engine;
    document.querySelectorAll("#engine-chips .chip").forEach((c) => c.classList.toggle("on", c === btn));
    syncMode();
    if (form.engine.value === "pdu" && form.instance_id.value && !form.pgdata_path.value.trim()) {
      runDiscover({ fillPaths: true });
    }
  };
  document.getElementById("pdu-scenes").onclick = (ev) => {
    const btn = ev.target.closest("[data-scene]");
    if (!btn) return;
    form.pdu_scene.value = btn.dataset.scene;
    document.querySelectorAll("#pdu-scenes .scene").forEach((c) => c.classList.toggle("on", c === btn));
    syncMode();
  };
  const disc = document.getElementById("btn-discover");
  if (disc) disc.onclick = () => runDiscover({ fillPaths: true });
  syncMode();
  if (form.engine.value === "pdu" && form.instance_id.value && !form.pgdata_path.value.trim()) {
    runDiscover({ fillPaths: true });
  }
  document.getElementById("rel-chips").onclick = (ev) => {
    const btn = ev.target.closest(".chip[data-m]");
    if (!btn) return;
    document.querySelectorAll("#rel-chips .chip").forEach((c) => c.classList.toggle("on", c === btn));
    const m = Number(btn.dataset.m);
    const end = new Date();
    form.end_time.value = toLocalInput(end);
    form.target_time.value = toLocalInput(new Date(end.getTime() - m * 60 * 1000));
  };
  document.getElementById("btn-precheck").onclick = async () => {
    const msg = document.getElementById("form-msg");
    msg.textContent = "";
    const body = collectTaskForm(form);
    if (body.engine !== "pdu" && !body.instance_id) { msg.innerHTML = `<div class="err">请先选择实例地址</div>`; return; }
    if (body.engine === "pdu" && !body.pgdata_path) { msg.innerHTML = `<div class="err">请填写 PGDATA 副本路径</div>`; return; }
    if (body.engine === "pdu" && !body.target_time) { msg.innerHTML = `<div class="err">PDU 闪回必须填写起始时间</div>`; return; }
    try {
      const result = await api("/api/v1/flashback/tasks/precheck", { method: "POST", body: JSON.stringify(body) });
      wizard = { step: 2, precheck: result, form: body };
      renderPrecheckStep();
    } catch (err) {
      msg.innerHTML = `<div class="err">${esc(err.message)}</div>`;
    }
  };
}

function renderPrecheckStep() {
  setNav("new");
  const r = wizard.precheck || {};
  const items = r.items || [];
  app.innerHTML = `<section class="card">
    <h2>预检查</h2>
    ${stepsHTML(2, (wizard.form || {}).engine === "pdu")}
    <div class="info">${r.ok ? "预检查通过" : "预检查未通过"} · ${esc(r.host || "")}:${esc(r.port || "")} · ${esc(r.parse_mode || "")} · ${esc(r.server_version || "")}</div>
    <div class="checks" style="margin-top:14px">${items.map((it) => `<div class="check">${badge(it.status)}<div><strong>${esc(it.name)}</strong><div class="meta">${esc(it.message)}</div></div></div>`).join("")}</div>
    <div class="actions">
      <button class="btn ghost" id="back1">返回修改</button>
      <button class="btn primary" id="to3" ${r.ok ? "" : "disabled"}>下一步：执行确认</button>
    </div>
  </section>`;
  document.getElementById("back1").onclick = () => { wizard.step = 1; renderNew(); };
  document.getElementById("to3").onclick = () => { wizard.step = 3; renderConfirmStep(); };
}

function renderConfirmStep() {
  setNav("new");
  const f = wizard.form || {};
  const tables = !(f.tables || []).length ? "整库" : (f.tables.length === 1 ? "单表 " + f.tables[0] : "多表 " + f.tables.join(", "));
  app.innerHTML = `<section class="card">
    <h2>执行确认</h2>
    ${stepsHTML(3, f.engine === "pdu")}
    <div class="addr">
      ${f.engine === "pdu" ? `PDU ${esc(PDU_SCENE_CN[f.pdu_scene] || f.pdu_scene)} · ` : ""}实例 <strong>${esc(f.instance_id || "纯离线")}</strong> · 库 ${esc(f.database)} · 表 ${esc(tables)}<br>
      时间窗 ${esc(f.target_time)} → ${esc(f.end_time || "现在")} · ${esc(f.output_kind || "flashback")}
      ${f.engine === "pdu" ? `<br>PGDATA ${esc(f.pgdata_path || "")}${f.archive_dest ? " · WAL " + esc(f.archive_dest) : ""}${f.disk_path ? " · DISK " + esc(f.disk_path) : ""}` : ""}
    </div>
    <div class="actions">
      <button class="btn ghost" id="back2">返回预检查</button>
      <button class="btn primary" id="btn-create">${f.engine === "pdu" ? "开始离线处理" : "执行闪回"}</button>
    </div>
    <div id="form-msg"></div>
  </section>`;
  document.getElementById("back2").onclick = () => renderPrecheckStep();
  document.getElementById("btn-create").onclick = async () => {
    const msg = document.getElementById("form-msg");
    try {
      const result = await api("/api/v1/flashback/tasks", { method: "POST", body: JSON.stringify(wizard.form) });
      location.hash = "#/tasks/" + encodeURIComponent(result.id);
    } catch (err) {
      msg.innerHTML = `<div class="err">${esc(err.message)}</div>`;
    }
  };
}

async function renderHistory() {
  setNav("history");
  const r = route();
  const status = r.q.get("status") || "";
  const keyword = r.q.get("keyword") || r.q.get("q") || "";
  const page = Number(r.q.get("page") || 1);
  app.innerHTML = `<section class="card">
    <div class="toolbar">
      <div class="grow"><h2>闪回历史</h2><p class="lead">任务地址来自已配置实例，不在这里改连接。</p></div>
      <a class="btn primary" href="#/">新建任务</a>
    </div>
    <div class="toolbar">
      <div class="field"><label>状态</label>
        <select id="flt-status">
          <option value="">全部</option>
          <option value="pending">排队</option>
          <option value="running">执行中</option>
          <option value="succeeded">成功</option>
          <option value="failed">失败</option>
        </select>
      </div>
      <div class="field grow"><label>关键字</label><input id="flt-keyword" value="${esc(keyword)}" placeholder="库名 / 实例 / 任务 ID"></div>
      <button class="btn ghost" id="flt-go">筛选</button>
    </div>
    <div id="hist"></div>
  </section>`;
  document.getElementById("flt-status").value = status;
  const applyFilter = () => {
    const next = new URLSearchParams();
    const st = document.getElementById("flt-status").value;
    const kw = document.getElementById("flt-keyword").value.trim();
    if (st) next.set("status", st);
    if (kw) next.set("keyword", kw);
    location.hash = "#/history" + (next.toString() ? "?" + next : "");
  };
  document.getElementById("flt-go").onclick = applyFilter;
  document.getElementById("flt-keyword").onkeydown = (ev) => {
    if (ev.key === "Enter") applyFilter();
  };
  try {
    const qs = new URLSearchParams({ page: String(page), page_size: String(HISTORY_PAGE_SIZE) });
    if (status) qs.set("status", status);
    if (keyword) qs.set("keyword", keyword);
    const data = await api("/api/v1/flashback/tasks?" + qs.toString());
    const list = (data && data.list) || [];
    const total = Number(data && data.total) || 0;
    const pages = Math.max(1, Math.ceil(total / HISTORY_PAGE_SIZE));
    const box = document.getElementById("hist");
    const jump = (p) => {
      const next = new URLSearchParams(r.q);
      next.set("page", String(Math.min(pages, Math.max(1, p))));
      location.hash = "#/history?" + next.toString();
    };
    if (total > 0 && page > pages) {
      jump(pages);
      return;
    }
    if (!list.length) { box.innerHTML = `<div class="empty">还没有历史任务</div>`; return; }
    const nums = historyPageItems(page, pages).map((it) => it.dots
      ? `<span class="meta">…</span>`
      : `<button type="button" class="chip ${it.p === page ? "on" : ""}" data-page="${it.p}">${esc(it.label)}</button>`).join("");
    box.innerHTML = `<table class="data"><thead><tr><th>时间</th><th>模式</th><th>实例 / 地址</th><th>库 / 表</th><th>闪回点</th><th>状态</th><th>操作</th></tr></thead><tbody>
      ${list.map((t) => `<tr>
        <td>${esc(fmtTime(t.created_at))}</td>
        <td>${t.engine === "pdu" ? badge("pdu") + " " + esc(PDU_SCENE_CN[t.pdu_scene] || "离线") : badge("online")}</td>
        <td>${esc(t.instance_id)}<div class="meta">${t.engine === "pdu" ? esc(t.pgdata_path || "本机") : esc(t.host) + ":" + esc(t.port)}</div></td>
        <td>${esc(t.database)}<div class="meta">${esc((t.tables || []).join(", ") || "整库")}</div></td>
        <td>${esc(fmtTime(t.target_time))}</td>
        <td>${badge(t.status)}</td>
        <td><a class="btn ghost small" href="#/tasks/${encodeURIComponent(t.id)}">查看</a></td>
      </tr>`).join("")}
    </tbody></table>
    <div class="pager">
      <span>共 ${total} 条 · 每页 ${HISTORY_PAGE_SIZE} 条 · 第 ${page} / ${pages} 页</span>
      <div class="pager-btns">
        <button class="btn ghost small" ${page <= 1 ? "disabled" : ""} id="prev">上一页</button>
        ${nums}
        <button class="btn ghost small" ${page >= pages ? "disabled" : ""} id="next">下一页</button>
      </div>
    </div>`;
    const prev = document.getElementById("prev");
    const next = document.getElementById("next");
    if (prev) prev.onclick = () => jump(page - 1);
    if (next) next.onclick = () => jump(page + 1);
    box.querySelectorAll("[data-page]").forEach((el) => {
      el.onclick = () => jump(Number(el.dataset.page));
    });
  } catch (err) {
    document.getElementById("hist").innerHTML = `<div class="err">${esc(err.message)}</div>`;
  }
}

async function renderInstances() {
  setNav("instances");
  app.innerHTML = `<section class="card"><div class="empty">加载实例地址…</div></section>`;
  let list = [];
  try { list = await loadInstances(); } catch (err) {
    app.innerHTML = `<section class="card"><div class="err">${esc(err.message)}</div></section>`;
    return;
  }
  const rows = list.map((it) => `<tr>
    <td class="mono">${esc(it.id)}</td>
    <td>${esc(it.db_type || "")}</td>
    <td>${esc(it.host)}:${esc(it.port)}</td>
    <td>${esc(it.user || "—")}</td>
    <td>${esc(it.vendor || "自建")}</td>
    <td>${badge(it.source || "yaml")}</td>
    <td>
      <button class="btn ghost small" data-edit="${esc(it.id)}">编辑</button>
      ${it.source === "db" ? `<button class="btn danger small" data-del="${esc(it.id)}">删除</button>` : `<span class="meta">YAML</span>`}
    </td>
  </tr>`).join("");
  app.innerHTML = `<section class="card">
    <div class="toolbar">
      <div class="grow"><h2>实例地址管理</h2><p class="lead">闪回任务只从这里选地址，不在任务页手填主机端口。</p></div>
    </div>
    ${list.length ? `<table class="data"><thead><tr><th>ID</th><th>类型</th><th>地址</th><th>用户</th><th>厂商</th><th>来源</th><th></th></tr></thead><tbody>${rows}</tbody></table>` : `<div class="empty">还没有地址。下面表单添加一条后即可被任务引用。</div>`}
    <h3 style="margin:22px 0 10px">新增 / 覆盖到地址库</h3>
    <form id="inst-form" class="grid">
      <div class="field"><label>ID *</label><input name="id" required placeholder="pg-prod"></div>
      <div class="field"><label>类型</label>
        <select name="db_type"><option value="postgres">PostgreSQL</option><option value="mysql">MySQL</option></select>
      </div>
      <div class="field"><label>主机 *</label><input name="host" required placeholder="10.100.112.17"></div>
      <div class="field"><label>端口</label><input name="port" type="number" value="5432"></div>
      <div class="field"><label>用户</label><input name="user" placeholder="postgres"></div>
      <div class="field"><label>密码</label><input name="password" type="password" placeholder="更新时留空则保留原密码"></div>
      <div class="field"><label>厂商</label>
        <select name="vendor">
          <option value="">自建</option>
          <option value="tencent">腾讯云</option>
          <option value="aliyun">阿里云</option>
          <option value="huawei">华为云</option>
          <option value="aws">AWS</option>
        </select>
      </div>
      <div class="field"><label>cloud_instance_id</label><input name="cloud_instance_id" placeholder="postgres-xxxx"></div>
      <div class="field"><label>region</label><input name="region" placeholder="ap-guangzhou"></div>
      <div class="field"><label>备注</label><input name="remark"></div>
      <div class="field"><label>SSH 用户</label><input name="ssh_user" placeholder="远程互信，可空=库用户"></div>
      <div class="field"><label>SSH 端口</label><input name="ssh_port" type="number" placeholder="22"></div>
    </form>
    <div class="actions"><button class="btn primary" id="btn-inst-save">保存到数据库</button></div>
    <div id="inst-msg"></div>
  </section>`;

  const fill = (it) => {
    const f = document.getElementById("inst-form");
    f.id.value = it.id || "";
    f.db_type.value = (it.db_type || "").includes("mysql") ? "mysql" : "postgres";
    f.host.value = it.host || "";
    f.port.value = it.port || 5432;
    f.user.value = it.user || "";
    f.password.value = "";
    f.vendor.value = it.vendor || "";
    f.cloud_instance_id.value = it.cloud_instance_id || "";
    f.region.value = it.region || "";
    f.remark.value = it.remark || "";
    f.ssh_user.value = it.ssh_user || "";
    f.ssh_port.value = it.ssh_port || "";
  };
  app.querySelectorAll("[data-edit]").forEach((btn) => {
    btn.onclick = () => fill(list.find((x) => x.id === btn.dataset.edit) || {});
  });
  app.querySelectorAll("[data-del]").forEach((btn) => {
    btn.onclick = async () => {
      if (!confirm("删除地址 " + btn.dataset.del + " ？")) return;
      try {
        await api("/api/v1/flashback/instances/" + encodeURIComponent(btn.dataset.del), { method: "DELETE" });
        renderInstances();
      } catch (err) {
        document.getElementById("inst-msg").innerHTML = `<div class="err">${esc(err.message)}</div>`;
      }
    };
  });
  document.getElementById("btn-inst-save").onclick = async () => {
    const f = document.getElementById("inst-form");
    const fd = new FormData(f);
    const body = {
      id: String(fd.get("id") || "").trim(),
      db_type: String(fd.get("db_type") || "postgres"),
      host: String(fd.get("host") || "").trim(),
      port: Number(fd.get("port") || 0),
      user: String(fd.get("user") || "").trim(),
      password: String(fd.get("password") || ""),
      vendor: String(fd.get("vendor") || ""),
      cloud_instance_id: String(fd.get("cloud_instance_id") || "").trim(),
      region: String(fd.get("region") || "").trim(),
      remark: String(fd.get("remark") || "").trim(),
      ssh_user: String(fd.get("ssh_user") || "").trim(),
      ssh_port: Number(fd.get("ssh_port") || 0),
    };
    const msg = document.getElementById("inst-msg");
    try {
      await api("/api/v1/flashback/instances/" + encodeURIComponent(body.id), { method: "PUT", body: JSON.stringify(body) });
      renderInstances();
    } catch (err) {
      msg.innerHTML = `<div class="err">${esc(err.message)}</div>`;
    }
  };
}

async function renderTask(id) {
  setNav("history");
  app.innerHTML = `<section class="card"><div class="empty">加载任务 ${esc(id)} …</div></section>`;
  try { await paintTask(id); } catch (err) {
    app.innerHTML = `<section class="card"><div class="err">${esc(err.message)}</div></section>`;
  }
}

const TASK_POLL_MS = 8000;

function taskRunning(status) {
  return status === "pending" || status === "running";
}

function taskMetersHTML(task) {
  const p = task.progress || {};
  if (task.engine === "pdu") {
    return `<div><div class="meta">${esc(STATUS_CN[p.phase] || p.phase || "处理")} ${p.parse_done || p.fetch_done || 0}/${p.parse_total || p.fetch_total || 0}</div><div class="track"><div class="bar parse" style="width:${p.parse_percent || p.fetch_percent || 0}%"></div></div></div>
      ${task.error_message ? `<div class="err">${esc(task.error_message)}</div>` : ""}`;
  }
  return `<div><div class="meta">获取日志 ${p.fetch_done || 0}/${p.fetch_total || 0}</div><div class="track"><div class="bar" style="width:${p.fetch_percent || 0}%"></div></div></div>
    <div><div class="meta">解析 ${p.parse_done || 0}/${p.parse_total || 0}</div><div class="track"><div class="bar parse" style="width:${p.parse_percent || 0}%"></div></div></div>
    ${task.error_message ? `<div class="err">${esc(task.error_message)}</div>` : ""}`;
}

function taskLogsHTML(logs) {
  return (logs || []).map((l) => `<p class="log-line ${esc((l.level || "").toLowerCase())}">[${esc(fmtTime(l.created_at))}] ${esc(l.level)} ${esc(l.message)}</p>`).join("") || "<p class='log-line'>暂无日志</p>";
}

function patchTaskLive(task, logs) {
  const st = document.getElementById("task-status");
  if (st) st.innerHTML = badge(task.status);
  const meters = document.getElementById("task-meters");
  if (meters) meters.innerHTML = taskMetersHTML(task);
  const logBox = document.getElementById("task-logs");
  if (logBox) logBox.innerHTML = taskLogsHTML(logs);
}

async function paintTask(id, soft) {
  const [task, logs] = await Promise.all([
    api("/api/v1/flashback/tasks/" + encodeURIComponent(id)),
    api("/api/v1/flashback/tasks/" + encodeURIComponent(id) + "/logs").catch(() => []),
  ]);
  const running = taskRunning(task.status);
  const root = document.getElementById("task-detail");
  if (soft && root && root.dataset.id === id) {
    patchTaskLive(task, logs);
  } else {
    const tables = (task.tables || []).join(", ") || "整库";
    app.innerHTML = `<section class="card" id="task-detail" data-id="${esc(task.id)}">
      <div class="toolbar">
        <div class="grow"><h2>任务详情</h2><p class="mono">${esc(task.id)}</p></div>
        <span id="task-status">${badge(task.status)}</span>
        <a class="btn ghost" href="#/history">返回历史</a>
      </div>
      <div class="split">
        <div class="addr">
          ${task.engine === "pdu" ? `PDU ${esc(PDU_SCENE_CN[task.pdu_scene] || "离线")}<br>PGDATA ${esc(task.pgdata_path || "")}${task.archive_dest ? "<br>WAL " + esc(task.archive_dest) : ""}` : `实例 ${esc(task.instance_id)} · ${esc(task.host)}:${esc(task.port)}`}
          <br>库 ${esc(task.database)} / ${esc(tables)}<br>时间 ${esc(fmtTime(task.target_time))} → ${esc(fmtTime(task.end_time))}
        </div>
        <div class="meters" id="task-meters">${taskMetersHTML(task)}</div>
      </div>
      <div id="art-box"></div>
      <div class="actions">
        <button class="btn ghost small" id="reload-sql">刷新 SQL</button>
        <button class="btn ghost small" id="copy-sql">复制选中 SQL</button>
        <button class="btn ghost small" id="dl-sql">下载选中 .sql</button>
        <span class="meta" id="sql-picked">未选</span>
      </div>
      <div id="sql-box"></div>
      <h3>运行日志</h3>
      <div class="logs" id="task-logs">${taskLogsHTML(logs)}</div>
      ${task.engine === "pdu" ? `<p class="meta">离线解码参考 PDU-PostgreSQLDataUnloader</p>` : ""}
    </section>`;
    await paintSQL(id);
    await paintArtifacts(id);
    document.getElementById("reload-sql").onclick = () => paintSQL(id);
  }
  stopPoll();
  if (running) {
    pollTimer = setInterval(() => paintTask(id, true).catch(() => {}), TASK_POLL_MS);
  } else if (soft) {
    await paintSQL(id);
    await paintArtifacts(id);
  }
}

function selectedSQLText(rows, root) {
  const idxs = [...root.querySelectorAll(".sql-pick:checked")].map((el) => Number(el.dataset.i));
  return idxs.map((i) => (rows[i] && rows[i].statement) || "").filter(Boolean)
    .map((s) => s.replace(/;\s*$/, "") + ";").join("\n") + (idxs.length ? "\n" : "");
}

function syncSQLPick(root) {
  const picks = root.querySelectorAll(".sql-pick");
  const n = [...picks].filter((el) => el.checked).length;
  const all = document.getElementById("sql-all");
  if (all) all.checked = picks.length > 0 && n === picks.length;
  const hint = document.getElementById("sql-picked");
  if (hint) hint.textContent = n ? `已选 ${n} 条` : "未选";
}

async function paintSQL(id) {
  const box = document.getElementById("sql-box");
  if (!box) return;
  const data = await api(`/api/v1/flashback/tasks/${encodeURIComponent(id)}/sql?page=${sqlPage}&page_size=50`);
  const rows = (data && data.list) || [];
  box.innerHTML = rows.length ? `<table class="data"><thead><tr>
    <th><input type="checkbox" id="sql-all" title="全选本页"></th>
    <th>#</th><th>op</th><th>表</th><th>SQL</th></tr></thead><tbody>
    ${rows.map((r, i) => `<tr>
      <td><input type="checkbox" class="sql-pick" data-i="${i}"></td>
      <td>${esc(r.seq)}</td><td>${badge(r.op || r.kind)}</td>
      <td>${esc([r.schema_name, r.table_name].filter(Boolean).join("."))}</td>
      <td><pre>${esc(r.statement)}</pre></td>
    </tr>`).join("")}
  </tbody></table>
  <div class="pager"><span>共 ${data.total} 条</span>
    <button class="btn ghost small" ${sqlPage <= 1 ? "disabled" : ""} id="sql-prev">上一页</button>
    <button class="btn ghost small" ${(sqlPage * 50) >= (data.total || 0) ? "disabled" : ""} id="sql-next">下一页</button>
  </div>` : `<div class="empty">还没有生成 SQL</div>`;
  const all = document.getElementById("sql-all");
  if (all) all.onchange = () => {
    box.querySelectorAll(".sql-pick").forEach((el) => { el.checked = all.checked; });
    syncSQLPick(box);
  };
  box.querySelectorAll(".sql-pick").forEach((el) => { el.onchange = () => syncSQLPick(box); });
  syncSQLPick(box);
  const sqlPrev = document.getElementById("sql-prev");
  const sqlNext = document.getElementById("sql-next");
  if (sqlPrev) sqlPrev.onclick = () => { sqlPage -= 1; paintSQL(id); };
  if (sqlNext) sqlNext.onclick = () => { sqlPage += 1; paintSQL(id); };
  const copyBtn = document.getElementById("copy-sql");
  const dlBtn = document.getElementById("dl-sql");
  if (copyBtn) copyBtn.onclick = async () => {
    const text = selectedSQLText(rows, box);
    if (!text) { alert("请先勾选要复制的 SQL"); return; }
    await navigator.clipboard.writeText(text);
    copyBtn.textContent = "已复制";
    setTimeout(() => { copyBtn.textContent = "复制选中 SQL"; }, 1200);
  };
  if (dlBtn) dlBtn.onclick = () => {
    const text = selectedSQLText(rows, box);
    if (!text) { alert("请先勾选要下载的 SQL"); return; }
    const a = document.createElement("a");
    a.href = URL.createObjectURL(new Blob([text], { type: "text/sql" }));
    a.download = `flashback-${id}.sql`;
    a.click();
  };
}

async function paintArtifacts(id) {
  const box = document.getElementById("art-box");
  if (!box) return;
  try {
    const list = await api(`/api/v1/flashback/tasks/${encodeURIComponent(id)}/artifacts`);
    if (!list || !list.length) { box.innerHTML = ""; return; }
    box.innerHTML = `<h3>产物</h3><table class="data"><thead><tr><th>类型</th><th>文件</th><th>大小</th><th>行数</th><th></th></tr></thead><tbody>
      ${list.map((a) => `<tr>
        <td>${esc(a.kind)}</td><td class="mono">${esc(a.name)}</td>
        <td>${esc(a.bytes)}</td><td>${esc(a.row_count)}</td>
        <td><a class="btn ghost small" href="/api/v1/flashback/tasks/${encodeURIComponent(id)}/artifacts/file?name=${encodeURIComponent(a.name)}">下载</a></td>
      </tr>`).join("")}
    </tbody></table>`;
  } catch (_) {
    box.innerHTML = "";
  }
}

function sourceLabel(src) {
  return { db: "已存数据库", env: "环境变量", yaml: "YAML" }[src] || "未配置";
}

async function renderOps() {
  setNav("ops");
  app.innerHTML = `<section class="card"><div class="empty">加载运维配置…</div></section>`;
  try {
    const settings = await api("/api/v1/flashback/cloud-settings");
    const vendors = (settings.vendors || []).map((v) => `<div class="check">${badge(v.configured ? "passed" : "failed")}<div><strong>${esc(v.name)}</strong><div class="mono">${esc(v.id_key)}</div></div></div>`).join("");
    const fields = (settings.args || []).map((a) => `<div class="field">
      <label>${esc(a.description || a.key)} · ${esc(sourceLabel(a.source))}</label>
      <input name="${esc(a.key)}" type="${a.secret ? "password" : "text"}" value="${esc(a.value)}" autocomplete="off">
    </div>`).join("");
    const allows = (settings.offline_allow_paths || []).join("、") || "未配置（将使用默认 /tmp /data /Users）";
    app.innerHTML = `<section class="card">
      <h2>运维中心 · 多云密钥</h2>
      <p class="lead">保存到 tbl_flashback_args，立即生效。</p>
      <div class="info">PDU 离线白名单 offline_allow_paths：${esc(allows)}</div>
      <div class="checks" style="margin:12px 0">${vendors}</div>
      <form id="cloud-form" class="grid">${fields}</form>
      <div class="actions"><button class="btn primary" id="btn-cloud-save">保存到数据库</button></div>
      <div id="cloud-msg"></div>
    </section>`;
    document.getElementById("btn-cloud-save").onclick = async () => {
      const fd = new FormData(document.getElementById("cloud-form"));
      const args = (settings.args || []).map((a) => ({ key: a.key, value: String(fd.get(a.key) || ""), description: a.description }));
      try {
        await api("/api/v1/flashback/cloud-settings", { method: "PUT", body: JSON.stringify({ args }) });
        document.getElementById("cloud-msg").innerHTML = `<p class="ok">已保存</p>`;
      } catch (err) {
        document.getElementById("cloud-msg").innerHTML = `<div class="err">${esc(err.message)}</div>`;
      }
    };
  } catch (err) {
    app.innerHTML = `<section class="card"><div class="err">${esc(err.message)}</div></section>`;
  }
}

async function renderTools() {
  setNav("tools");
  let instances = [];
  try { instances = await loadInstances(); } catch (err) {
    app.innerHTML = `<section class="card"><div class="err">${esc(err.message)}</div></section>`;
    return;
  }
  app.innerHTML = `<section class="card">
    <h2>工具与集成</h2>
    <p class="lead">连通自测使用已配置实例地址，不提交工单。</p>
    <form id="st-form" class="grid">
      <div class="field"><label>实例</label><select name="instance_id">${instanceOptions(instances)}</select></div>
      <div class="field"><label>数据库</label><input name="database" required placeholder="postgres"></div>
      <div class="field"><label>输出</label>
        <select name="output_kind"><option value="flashback">flashback</option><option value="original">original</option></select>
      </div>
    </form>
    <div class="actions"><button class="btn primary" id="btn-st">开始自测</button></div>
    <div id="st-msg"></div><div id="st-box"></div>
  </section>`;
  document.getElementById("btn-st").onclick = async (ev) => {
    const btn = ev.currentTarget;
    const fd = new FormData(document.getElementById("st-form"));
    btn.disabled = true;
    document.getElementById("st-msg").innerHTML = `<p class="lead">自测可能需要几分钟…</p>`;
    try {
      const result = await api("/api/v1/flashback/tasks/selftest", {
        method: "POST",
        body: JSON.stringify({
          instance_id: String(fd.get("instance_id") || "").trim(),
          database: String(fd.get("database") || "").trim(),
          output_kind: String(fd.get("output_kind") || "flashback"),
        }),
      });
      document.getElementById("st-msg").innerHTML = result.ok ? `<p class="ok">自测通过</p>` : `<p class="err">自测未通过</p>`;
      document.getElementById("st-box").innerHTML = (result.checks || []).map((c) => `<div class="check">${badge(c.ok ? "passed" : "failed")}<div><strong>${esc(c.name)}</strong><div class="meta">${esc(c.detail)}</div></div></div>`).join("");
    } catch (err) {
      document.getElementById("st-msg").innerHTML = `<div class="err">${esc(err.message)}</div>`;
    } finally { btn.disabled = false; }
  };
}

async function render() {
  stopPoll();
  sqlPage = 1;
  const r = route();
  if (r.name === "new") await renderNew();
  else if (r.name === "history") await renderHistory();
  else if (r.name === "instances") await renderInstances();
  else if (r.name === "ops") await renderOps();
  else if (r.name === "tools") await renderTools();
  else if (r.name === "task") await renderTask(r.id);
  refreshRecent();
}

document.getElementById("top-search").onsubmit = (ev) => {
  ev.preventDefault();
  const q = new FormData(ev.target).get("q");
  location.hash = "#/history?keyword=" + encodeURIComponent(String(q || "").trim());
};

window.addEventListener("hashchange", render);
pingHealth();
setInterval(pingHealth, 15000);
render();
