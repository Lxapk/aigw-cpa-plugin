package main

import "strings"

// mainPageScript returns the browser-side JavaScript for the combined page.
//
// The key handling is the same as the other pages: CPA's management endpoints
// authenticate from a request header and the resource route is GET-only, so the
// key lives in localStorage and is attached by fetch().
func mainPageScript() string {
	const script = `<script>
(function () {
  var KEY_NAME = '{{KEY_NAME}}';
  var MGMT = '{{MGMT}}';

  function key() {
    try { return localStorage.getItem(KEY_NAME) || ''; } catch (e) { return ''; }
  }

  function setKeyState(msg, cls) {
    var el = document.getElementById('keyState');
    if (!el) return;
    el.textContent = msg || '';
    el.className = 'muted ' + (cls || '');
  }

  function refreshKeyState() {
    var k = key();
    if (k) {
      setKeyState('已保存密钥（' + k.length + ' 字符），按钮可直接使用。', 'ok');
    } else {
      setKeyState('尚未保存密钥，操作会返回 “missing management key”。', 'warn');
    }
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
    if (!k) { return Promise.reject(new Error('请先在上方保存管理密钥')); }
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

  function renderCheckin(run) {
    if (!run) { return ''; }
    var out = '<div class="card"><div class="muted">签到 · 成功 ' + (run.succeeded || 0) +
      ' / 失败 ' + (run.failed || 0) + '</div>';
    var rs = run.results || [];
    if (rs.length) {
      out += '<table><tr><th>账号</th><th>结果</th><th>说明</th><th>码</th></tr>';
      rs.forEach(function (r) {
        var cls = 'ok', text = '成功';
        if (r.error) { cls = 'bad'; text = '错误'; }
        else if (!r.success) { cls = 'bad'; text = '失败'; }
        else if (r.already_checked_in) { cls = 'warn'; text = '已签到'; }
        out += '<tr><td>' + esc(r.label || r.auth_id) + '</td>' +
          '<td class="' + cls + '">' + text + '</td>' +
          '<td>' + esc(r.error || r.message) + '</td>' +
          '<td>' + (r.code == null ? '' : r.code) + '</td></tr>';
      });
      out += '</table>';
    }
    out += '</div>';
    return out;
  }

  function renderQuota(results) {
    if (!results || !results.length) { return ''; }
    var out = '<div class="card"><div class="muted">额度</div>' +
      '<table><tr><th>账号</th><th>区域</th><th>剩余额度</th><th>说明</th></tr>';
    results.forEach(function (r) {
      var cls = r.error ? 'bad' : 'ok';
      out += '<tr><td>' + esc(r.label || r.auth_id) + '</td>' +
        '<td>' + esc(r.region) + '</td>' +
        '<td class="' + cls + '">' + (r.credits == null ? 0 : r.credits) + '</td>' +
        '<td>' + esc(r.error || r.message) + '</td></tr>';
    });
    out += '</table></div>';
    return out;
  }

  // One button for the common flow: check in, then refresh quota, then reload
  // so the account table shows the new numbers.
  window.runAll = function () {
    var btn = document.getElementById('btnRun');
    var msg = document.getElementById('runMsg');
    var box = document.getElementById('runResult');
    if (btn) btn.disabled = true;
    if (box) box.innerHTML = '';
    if (msg) { msg.textContent = '执行中，请稍候…'; msg.className = 'muted'; }

    call('{{RUN}}', { method: 'POST' }).then(function (payload) {
      if (msg) {
        var c = payload.checkin || {};
        msg.textContent = '完成：签到 成功 ' + (c.succeeded || 0) + ' / 失败 ' + (c.failed || 0);
        msg.className = 'ok';
      }
      if (box) {
        box.innerHTML = renderCheckin(payload.checkin) + renderQuota(payload.quota);
      }
      setTimeout(function () { location.reload(); }, 1200);
    }).catch(function (e) {
      if (msg) { msg.textContent = '执行失败：' + e.message; msg.className = 'bad'; }
    }).then(function () {
      if (btn) btn.disabled = false;
    });
  };

  // Account-switching strategy.
  window.saveStrategy = function () {
    var msg = document.getElementById('strategyMsg');
    var picked = document.querySelector('input[name="strategy"]:checked');
    if (!picked) { if (msg) { msg.textContent = '请选择一种策略'; msg.className = 'bad'; } return; }
    if (msg) { msg.textContent = '应用中…'; msg.className = 'muted'; }
    call('{{ROUTING_CONFIG}}', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ strategy: picked.value })
    }).then(function () {
      if (msg) { msg.textContent = '已应用：' + picked.value; msg.className = 'ok'; }
      setTimeout(function () { location.reload(); }, 600);
    }).catch(function (e) {
      if (msg) { msg.textContent = '应用失败：' + e.message; msg.className = 'bad'; }
    });
  };

  window.resetRotation = function () {
    var msg = document.getElementById('strategyMsg');
    if (msg) { msg.textContent = '重置中…'; msg.className = 'muted'; }
    call('{{ROTATION_RESET}}', { method: 'POST' }).then(function () {
      if (msg) { msg.textContent = '轮巡位置已重置'; msg.className = 'ok'; }
    }).catch(function (e) {
      if (msg) { msg.textContent = '重置失败：' + e.message; msg.className = 'bad'; }
    });
  };

  window.saveSettings = function () {
    var msg = document.getElementById('runMsg');
    if (msg) { msg.textContent = '保存中…'; msg.className = 'muted'; }

    var checkin = {
      enabled: !!document.getElementById('ckEnabled').checked,
      hour: parseInt(document.getElementById('ckHour').value, 10) || 0,
      minute: parseInt(document.getElementById('ckMinute').value, 10) || 0,
      on_start: !!document.getElementById('ckOnStart').checked
    };
    var quota = {
      enabled: !!document.getElementById('qEnabled').checked,
      interval_minutes: parseInt(document.getElementById('qInterval').value, 10) || 30,
      refresh_on_start: !!document.getElementById('qOnStart').checked
    };

    Promise.all([
      call('{{CK_CONFIG}}', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(checkin)
      }),
      call('{{Q_CONFIG}}', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(quota)
      })
    ]).then(function () {
      if (msg) { msg.textContent = '设置已保存'; msg.className = 'ok'; }
      setTimeout(function () { location.reload(); }, 600);
    }).catch(function (e) {
      if (msg) { msg.textContent = '保存失败：' + e.message; msg.className = 'bad'; }
    });
  };

  document.addEventListener('DOMContentLoaded', refreshKeyState);
  if (document.readyState !== 'loading') refreshKeyState();
})();
</script>`

	return strings.NewReplacer(
		"{{KEY_NAME}}", checkinKeyStorageName,
		"{{MGMT}}", managementBasePath()+"/"+pluginName,
		"{{RUN}}", managementBasePath()+"/"+pluginName+"/run",
		"{{CK_CONFIG}}", managementBasePath()+"/"+pluginName+"/checkin/config",
		"{{Q_CONFIG}}", managementBasePath()+"/"+pluginName+"/quota/config",
		"{{ROUTING_CONFIG}}", managementBasePath()+"/"+pluginName+"/routing/config",
		"{{ROTATION_RESET}}", managementBasePath()+"/"+pluginName+"/routing/reset",
	).Replace(script)
}
