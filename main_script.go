package main

import "strings"

// mainPageScript returns the browser-side JavaScript for the combined page.
//
// The key handling is deliberate: CPA's management endpoints authenticate from a
// request header and the resource route is GET-only, so the key lives in
// localStorage and is attached by fetch(). It never reaches the plugin.
func mainPageScript() string {
	const script = `<script>
{{UI_TABS}}
(function () {
  var KEY_NAME = '{{KEY_NAME}}';
  var MGMT = '{{MGMT}}';
  var BASE = '{{BASE}}';

  function key() {
    try { return localStorage.getItem(KEY_NAME) || ''; } catch (e) { return ''; }
  }

  function setKeyState(msg, cls) {
    var el = document.getElementById('keyState');
    if (!el) return;
    el.textContent = msg || '';
    el.className = 'muted small ' + (cls || '');
  }

  function refreshKeyState() {
    var k = key();
    if (k) setKeyState('已保存密钥（' + k.length + ' 字符），操作可直接使用。', 'ok');
    else setKeyState('尚未保存密钥，涉及数据的操作会提示缺少管理密钥。', 'warn');
  }

  window.saveKey = function () {
    var input = document.getElementById('mgmtKey');
    var v = (input && input.value || '').trim();
    if (!v) { setKeyState('请输入密钥', 'bad'); return; }
    try { localStorage.setItem(KEY_NAME, v); } catch (e) {
      setKeyState('浏览器拒绝保存（可能禁用了 localStorage）', 'bad'); return;
    }
    if (input) input.value = '';
    refreshKeyState();
  };

  window.clearKey = function () {
    try { localStorage.removeItem(KEY_NAME); } catch (e) {}
    refreshKeyState();
  };

  function call(path, options) {
    var k = key();
    if (!k) { return Promise.reject(new Error('请先在「设置」里保存管理密钥')); }
    var opts = options || {};
    opts.headers = Object.assign({
      'Authorization': 'Bearer ' + k,
      'X-Management-Key': k
    }, opts.headers || {});
    return fetch(path, opts).then(function (resp) {
      return resp.text().then(function (body) {
        var data = null;
        try { data = JSON.parse(body); } catch (e) {}
        if (!resp.ok) {
          var msg = (data && (data.error || data.message)) || ('HTTP ' + resp.status);
          if (String(msg).indexOf('management key') >= 0) {
            msg = '管理密钥无效或未配置（' + msg + '）';
          }
          throw new Error(msg);
        }
        return data;
      });
    });
  }

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  function msgSet(id, text, cls) {
    var el = document.getElementById(id);
    if (!el) return;
    el.textContent = text || '';
    el.className = 'small ' + (cls || 'muted');
  }

  function renderCheckin(run) {
    if (!run) return '';
    var rs = run.results || [];
    var out = '<table><thead><tr><th>账号</th><th>结果</th><th>说明</th><th class="num">码</th></tr></thead><tbody>';
    if (!rs.length) {
      out += '<tr><td colspan="4" class="muted">无结果</td></tr>';
    }
    rs.forEach(function (r) {
      var pill = 'ok', text = '成功';
      if (r.error) { pill = 'bad'; text = '错误'; }
      else if (!r.success) { pill = 'bad'; text = '失败'; }
      else if (r.already_checked_in) { pill = 'warn'; text = '已签到'; }
      out += '<tr><td>' + esc(r.label || r.auth_id) + '</td>' +
        '<td><span class="pill ' + pill + '">' + text + '</span></td>' +
        '<td>' + esc(r.error || r.message) + '</td>' +
        '<td class="num">' + (r.code == null ? '' : r.code) + '</td></tr>';
    });
    out += '</tbody></table>';
    return out;
  }

  function renderQuota(results) {
    if (!results || !results.length) return '';
    var out = '<table><thead><tr><th>账号</th><th>区域</th><th class="num">剩余积分</th><th>说明</th></tr></thead><tbody>';
    results.forEach(function (r) {
      var cls = r.error ? 'bad' : 'ok';
      out += '<tr><td>' + esc(r.label || r.auth_id) + '</td>' +
        '<td><span class="pill idle">' + esc(r.region) + '</span></td>' +
        '<td class="num ' + cls + '">' + (r.credits == null ? 0 : r.credits) + '</td>' +
        '<td>' + esc(r.error || r.message) + '</td></tr>';
    });
    out += '</tbody></table>';
    return out;
  }

  // ---- combined action ------------------------------------------------
  window.runAll = function () {
    var btn = document.getElementById('btnRun');
    var box = document.getElementById('runResult');
    if (btn) btn.disabled = true;
    if (box) box.innerHTML = '';
    msgSet('runMsg', '执行中…', 'muted');

    call(BASE + '/run', { method: 'POST' }).then(function (payload) {
      var c = payload.checkin || {};
      msgSet('runMsg', '完成：签到成功 ' + (c.succeeded || 0) + ' / 失败 ' + (c.failed || 0), 'ok');
      if (box) {
        box.innerHTML = '<h2>本次结果</h2>' +
          (payload.checkin ? renderCheckin(payload.checkin) : '') +
          (payload.quota ? renderQuota(payload.quota) : '');
      }
      setTimeout(function () { location.reload(); }, 1500);
    }).catch(function (e) {
      msgSet('runMsg', '执行失败：' + e.message, 'bad');
    }).then(function () { if (btn) btn.disabled = false; });
  };

  window.refreshAccounts = function () {
    msgSet('runMsg', '刷新中…', 'muted');
    call(BASE + '/accounts').then(function (d) {
      msgSet('runMsg', '账号 ' + (d.total || 0) + ' 个，可用 ' + (d.usable || 0) + ' 个', 'ok');
      setTimeout(function () { location.reload(); }, 700);
    }).catch(function (e) { msgSet('runMsg', '刷新失败：' + e.message, 'bad'); });
  };

  // ---- strategy --------------------------------------------------------
  window.saveStrategy = function () {
    var picked = document.querySelector('input[name="strategy"]:checked');
    if (!picked) { msgSet('strategyMsg', '请选择一种策略', 'bad'); return; }
    msgSet('strategyMsg', '应用中…', 'muted');
    call(BASE + '/routing/config', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ strategy: picked.value })
    }).then(function () {
      msgSet('strategyMsg', '已应用：' + picked.value, 'ok');
      setTimeout(function () { location.reload(); }, 700);
    }).catch(function (e) { msgSet('strategyMsg', '应用失败：' + e.message, 'bad'); });
  };

  window.resetRotation = function () {
    msgSet('strategyMsg', '重置中…', 'muted');
    call(BASE + '/routing/reset', { method: 'POST' }).then(function () {
      msgSet('strategyMsg', '轮巡位置已重置', 'ok');
    }).catch(function (e) { msgSet('strategyMsg', '重置失败：' + e.message, 'bad'); });
  };

  // ---- check-in --------------------------------------------------------
  window.runCheckin = function () {
    msgSet('runMsg', '签到中…', 'muted');
    call(BASE + '/checkin/run', { method: 'POST' }).then(function (run) {
      msgSet('runMsg', '签到完成：成功 ' + (run.succeeded || 0) + ' / 失败 ' + (run.failed || 0), 'ok');
      var box = document.getElementById('runResult');
      if (box) box.innerHTML = '<h2>签到结果</h2>' + renderCheckin(run);
      setTimeout(function () { location.reload(); }, 1500);
    }).catch(function (e) { msgSet('runMsg', '签到失败：' + e.message, 'bad'); });
  };

  window.saveCheckinSettings = function () {
    var msg = document.getElementById('runMsg');
    if (msg) { msg.textContent = '保存中…'; msg.className = 'small muted'; }
    call(BASE + '/checkin/config', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        enabled: !!document.getElementById('ckEnabled').checked,
        hour: parseInt(document.getElementById('ckHour').value, 10) || 0,
        minute: parseInt(document.getElementById('ckMinute').value, 10) || 0,
        on_start: !!document.getElementById('ckOnStart').checked
      })
    }).then(function () {
      if (msg) { msg.textContent = '设置已保存'; msg.className = 'small ok'; }
      setTimeout(function () { location.reload(); }, 700);
    }).catch(function (e) {
      if (msg) { msg.textContent = '保存失败：' + e.message; msg.className = 'small bad'; }
    });
  };

  // ---- quota -----------------------------------------------------------
  window.refreshQuota = function () {
    msgSet('runMsg', '查询中…', 'muted');
    call(BASE + '/quota/refresh', { method: 'POST' }).then(function (payload) {
      msgSet('runMsg', '完成：积分合计 ' + (payload.total_credits || 0) +
        '（' + (payload.accounts_known || 0) + '/' + (payload.accounts_total || 0) + ' 账号已查询）', 'ok');
      var box = document.getElementById('runResult');
      if (box) box.innerHTML = '<h2>积分结果</h2>' + renderQuota(payload.results);
      setTimeout(function () { location.reload(); }, 1500);
    }).catch(function (e) { msgSet('runMsg', '刷新失败：' + e.message, 'bad'); });
  };

  window.saveQuotaSettings = function () {
    var msg = document.getElementById('runMsg');
    if (msg) { msg.textContent = '保存中…'; msg.className = 'small muted'; }
    call(BASE + '/quota/config', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        enabled: !!document.getElementById('qEnabled').checked,
        interval_minutes: parseInt(document.getElementById('qInterval').value, 10) || 30,
        refresh_on_start: !!document.getElementById('qOnStart').checked
      })
    }).then(function () {
      if (msg) { msg.textContent = '设置已保存'; msg.className = 'small ok'; }
      setTimeout(function () { location.reload(); }, 700);
    }).catch(function (e) {
      if (msg) { msg.textContent = '保存失败：' + e.message; msg.className = 'small bad'; }
    });
  };

  // ---- variant override -----------------------------------------------
  window.setVariant = function (v) {
    var msg = document.getElementById('variantMsg');
    if (msg) { msg.textContent = '保存中…'; msg.className = 'small muted'; }
    call(BASE + '/variant', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ variant: v })
    }).then(function () {
      // Update the segmented control in place. Reloading immediately used to
      // wipe this confirmation after 700ms, which is why a successful switch
      // looked like nothing had happened.
      var seg = document.getElementById('variantSeg');
      if (seg) {
        var buttons = seg.getElementsByTagName('button');
        for (var i = 0; i < buttons.length; i++) {
          buttons[i].className = buttons[i].getAttribute('data-variant') === (v || 'auto') ? 'active' : '';
        }
      }
      if (msg) { msg.textContent = '已切换为 ' + (v === 'cn' ? '国内版' : v === 'ai' ? '国际版' : '自动识别') + '，账号归属已按新设置重新计算'; msg.className = 'small ok'; }
      setTimeout(function () { location.reload(); }, 2500);
    }).catch(function (e) {
      if (msg) { msg.textContent = '设置失败：' + e.message; msg.className = 'small bad'; }
    });
  };

  // ---- account toggle --------------------------------------------------
  // toggleAccount enables or disables one account.
  //
  // Feedback goes to #accountMsg, which lives in the accounts tab. The previous
  // version wrote to #runMsg — an element in a different tab — so a successful
  // toggle produced no visible change at all and read as "禁用没生效".
  window.toggleAccount = function (uid, action, authIndex) {
    msgSet('accountMsg', '操作中…', 'muted');
    call(BASE + '/account/toggle', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ uid: uid, auth_index: authIndex || '', action: action || 'toggle' })
    }).then(function () {
      msgSet('accountMsg', action === 'enable' ? '已启用，正在刷新列表…' : '已禁用，正在刷新列表…', 'ok');
      setTimeout(function () { location.reload(); }, 500);
    }).catch(function (e) {
      msgSet('accountMsg', '操作失败：' + e.message, 'bad');
    });
  };

  // ---- task tab --------------------------------------------------------
  //
  // These two handlers are referenced by the task tab markup. They were
  // missing entirely, so every button on that tab threw a ReferenceError and
  // the page showed nothing at all — the "任务 tab 点了没反应" report.
  window.runAllTasks = function () {
    var btn = document.getElementById('btnRunAllTasks');
    var box = document.getElementById('taskResult');
    if (btn) btn.disabled = true;
    if (box) box.innerHTML = '';
    msgSet('taskMsg', '执行中…', 'muted');

    call(BASE + '/run', { method: 'POST' }).then(function (payload) {
      var c = payload.checkin || {};
      var extra = (c.skipped || 0) > 0 ? '，跳过 ' + c.skipped + ' 个' : '';
      msgSet('taskMsg', '完成：签到成功 ' + (c.succeeded || 0) + ' / 失败 ' + (c.failed || 0) + extra, 'ok');
      if (box) {
        box.innerHTML = '<h2>本次结果</h2>' +
          (payload.checkin ? renderCheckin(payload.checkin) : '') +
          (payload.quota ? renderQuota(payload.quota) : '');
      }
      setTimeout(function () { location.reload(); }, 1500);
    }).catch(function (e) {
      msgSet('taskMsg', '执行失败：' + e.message, 'bad');
    }).then(function () { if (btn) btn.disabled = false; });
  };

  // toggleAccountTask flips one account's task participation.
  //
  // It forwards to the same /account/toggle endpoint the account tab uses, so
  // the task tab and the account tab can never disagree about an account's
  // state. The caller passes the action to apply, not the current state.
  window.toggleAccountTask = function (uid, action) {
    msgSet('taskMsg', '操作中…', 'muted');
    call(BASE + '/account/toggle', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ uid: uid, action: action === 'enable' ? 'enable' : 'disable' })
    }).then(function () {
      msgSet('taskMsg', action === 'enable' ? '已启用，正在刷新…' : '已禁用，正在刷新…', 'ok');
      setTimeout(function () { location.reload(); }, 500);
    }).catch(function (e) {
      msgSet('taskMsg', '操作失败：' + e.message, 'bad');
    });
  };

  // ---- auto refresh ----------------------------------------------------
  //
  // The account list used to update only when the operator pressed 刷新列表.
  // A login completed in CPA's own Auth page therefore stayed invisible until a
  // manual reload, which read as "账号不同步". Poll the inventory while the
  // accounts tab is visible; the host call is cached for 5s server-side, so a
  // 20s interval is cheap.
  var AUTO_REFRESH_MS = 20000;
  var autoRefreshTimer = null;

  function accountsTabVisible() {
    var panel = document.getElementById('tab-accounts');
    return !!panel && panel.classList.contains('active');
  }

  function pollAccounts() {
    if (!key() || !accountsTabVisible()) return;
    call(BASE + '/accounts').then(function (d) {
      var stamp = document.getElementById('accountsStamp');
      if (stamp) {
        stamp.textContent = '账号 ' + (d.total || 0) + ' 个，可用 ' + (d.usable || 0) +
          ' 个 · 数据读取于 ' + new Date().toLocaleTimeString();
      }
      // Only reload when the inventory actually changed, so a steady state
      // does not keep yanking the page out from under the operator.
      var current = document.getElementById('accountsSignature');
      var list = d.accounts || [];
      var usableCount = 0;
      var parts = [];
      for (var i = 0; i < list.length; i++) {
        if (list[i].usable) usableCount++;
        parts.push((list[i].uid || list[i].auth_index || '') + (list[i].disabled_by_user ? 'D' : 'E'));
      }
      // Must match accountsSignature() in main_page.go exactly.
      var signature = list.length + ':' + usableCount + ':' + parts.join(',');
      if (current && current.value && current.value !== signature) {
        location.reload();
        return;
      }
      if (current) current.value = signature;
    }).catch(function () { /* transient; the next tick retries */ });
  }

  function startAutoRefresh() {
    if (autoRefreshTimer) return;
    autoRefreshTimer = setInterval(pollAccounts, AUTO_REFRESH_MS);
  }

  // ---- growth tasks ----------------------------------------------------
  //
  // The growth pass is slower than the other tabs' actions (it spaces upstream
  // calls by a second), so the buttons report progress and stay disabled until
  // the response arrives.
  function growthButton(id, busy, label) {
    var btn = document.getElementById(id);
    if (!btn) return;
    btn.disabled = busy;
    if (label) btn.textContent = label;
  }

  window.runGrowthTasks = function () {
    growthButton('btnRunGrowth', true, '执行中…');
    msgSet('taskMsg', '正在接取、点亮并领取成长任务（可能需要一两分钟）…', 'muted');

    call(BASE + '/growth/run', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ uid: 'all' })
    }).then(function (payload) {
      if (payload.ok === false) {
        msgSet('taskMsg', '执行失败：' + (payload.error || '未知原因'), 'bad');
        return;
      }
      var lines = payload.logs || [];
      var earned = payload.earned_credit || 0;
      msgSet('taskMsg', '完成：' + (payload.accounts_count || 0) + ' 个账号，累计 +' + earned + ' 积分', 'ok');
      var box = document.getElementById('taskResult');
      if (box) {
        var html = '<div class="card"><h2>成长任务结果 <span class="hint">+' + earned + ' 积分</span></h2><pre class="log">';
        for (var i = 0; i < lines.length; i++) {
          html += escapeHTML(lines[i].message) + '\n';
        }
        html += '</pre></div>';
        box.innerHTML = html;
      }
      setTimeout(function () { location.reload(); }, 2500);
    }).catch(function (e) {
      msgSet('taskMsg', '执行失败：' + e.message, 'bad');
    }).then(function () {
      growthButton('btnRunGrowth', false, '完成成长任务');
    });
  };

  window.runTravel = function () {
    growthButton('btnTravel', true, '执行中…');
    msgSet('taskMsg', '正在检查猫猫旅行…', 'muted');

    call(BASE + '/growth/travel', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ uid: 'all' })
    }).then(function (payload) {
      if (payload.ok === false) {
        msgSet('taskMsg', '执行失败：' + (payload.error || '未知原因'), 'bad');
        return;
      }
      var results = payload.results || [];
      var parts = [];
      for (var i = 0; i < results.length; i++) {
        parts.push(results[i].label + ': ' + (results[i].message || results[i].error || ''));
      }
      msgSet('taskMsg', parts.join('；') || '没有可执行的账号', parts.length ? 'ok' : 'muted');
    }).catch(function (e) {
      msgSet('taskMsg', '执行失败：' + e.message, 'bad');
    }).then(function () {
      growthButton('btnTravel', false, '猫猫旅行');
    });
  };

  // loadGrowthTasks renders the per-task detail for the first eligible account.
  window.loadGrowthTasks = function () {
    msgSet('growthMsg', '查询中…', 'muted');
    var box = document.getElementById('growthDetail');
    if (box) box.innerHTML = '';

    call(BASE + '/growth/tasks').then(function (runs) {
      var list = runs.runs || [];
      if (!list.length) {
        msgSet('growthMsg', '还没有运行记录，先执行一次成长任务', 'muted');
        return;
      }
      // The stored results carry the uid to query.
      return loadGrowthDetailFor(list[0].uid, list[0].label);
    }).catch(function (e) {
      msgSet('growthMsg', '查询失败：' + e.message, 'bad');
    });
  };

  function loadGrowthDetailFor(uid, label) {
    return call(BASE + '/growth/tasks?uid=' + encodeURIComponent(uid)).then(function (payload) {
      var box = document.getElementById('growthDetail');
      if (payload.ok === false) {
        msgSet('growthMsg', '查询失败：' + (payload.error || '未知原因') +
          (payload.detail ? ' — ' + payload.detail : ''), 'bad');
        if (box) box.innerHTML = '<div class="note bad">' + escapeHTML(payload.error || '') +
          (payload.detail ? '<br>' + escapeHTML(payload.detail) : '') + '</div>';
        return;
      }
      var tasks = payload.tasks || [];
      var s = payload.summary || {};
      var travel = s.travel || {};
      msgSet('growthMsg', '账号 ' + (payload.label || label) + '：能量 ' + (s.energy || 0) +
        '，连续打卡 ' + (s.streak_days || 0) + ' 天，猫猫 ' + (travel.state || '未知'), 'ok');
      if (box) {
        var html = '<table><thead><tr><th>任务</th><th class="num">进度</th><th class="num">奖励</th><th>状态</th></tr></thead><tbody>';
        for (var i = 0; i < tasks.length; i++) {
          var t = tasks[i];
          var statusText = t.status || '';
          if (t.unforgeable) statusText = '无法代做';
          else if (t.desktop_only) statusText = '需桌面操作';
          else if (statusText === 'claimed') statusText = '已领奖';
          else if (statusText === 'completed') statusText = '已完成';
          else if (statusText === 'not_accepted') statusText = '未接取';
          else if (statusText === 'accepted') statusText = '进行中';

          var note = '';
          if (t.skip_reason) note = ' <span class="muted small">' + escapeHTML(t.skip_reason) + '</span>';
          if (t.jump_url) note += ' <span class="muted small">' + escapeHTML(t.jump_url) + '</span>';

          html += '<tr><td><strong>' + escapeHTML(t.name || t.task_code) + '</strong>' + note + '</td>' +
            '<td class="num">' + (t.current || 0) + '/' + (t.target || 1) + '</td>' +
            '<td class="num">+' + (t.reward_credit || 0) + '</td>' +
            '<td>' + escapeHTML(statusText) + '</td></tr>';
        }
        html += '</tbody></table>';
        box.innerHTML = html;
      }
    });
  }

  // escapeHTML mirrors the server-side html.EscapeString for values that arrive
  // as JSON and are interpolated into markup.
  function escapeHTML(v) {
    return String(v == null ? '' : v)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
  }

  document.addEventListener('DOMContentLoaded', function () {
    refreshKeyState();
    restoreTab();
    startAutoRefresh();
    pollAccounts();
  });
  if (document.readyState !== 'loading') {
    refreshKeyState();
    restoreTab();
    startAutoRefresh();
    pollAccounts();
  }
})();
</script>`

	return strings.NewReplacer(
		"{{KEY_NAME}}", checkinKeyStorageName,
		"{{MGMT}}", managementBasePath()+"/"+pluginName,
		"{{BASE}}", managementBasePath()+"/"+pluginName,
		"{{UI_TABS}}", uiTabsScript,
	).Replace(script)
}
