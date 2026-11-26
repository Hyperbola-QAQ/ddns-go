(() => {
  const byId = id => document.getElementById(id);
  const editor = byId('memberEditor');
  const members = byId('nodes');
  const token = byId('token');
  const remove = byId('removeMember');
  const add = byId('add');
  const saveAudit = byId('saveAudit');
  let settings;
  let selected;
  let busy = false;
  let removing = false;

  function syncStatus(data) {
    const errors = data.SyncErrors || {};
    for (const row of members.children) {
      const status = row.querySelector('[data-sync]');
      if (!status) continue;
      status.textContent = row.dataset.id === data.Cluster.LocalID ? '本机' :
        (errors[row.dataset.id] || (data.SyncPending ? '同步检查中' : '暂无同步错误'));
    }
    return Object.keys(errors).length > 0;
  }

  function render(data) {
    settings = data;
    close();
    add.after(editor);
    members.replaceChildren();
    for (const node of data.Cluster.Nodes) {
      const local = node.ID === data.Cluster.LocalID;
      const row = document.createElement('section');
      row.dataset.id = node.ID;
      row.className = 'border-top py-3';
      const summary = document.createElement('div');
      summary.className = 'd-flex flex-wrap justify-content-between align-items-center';
      const identity = document.createElement('div');
      identity.style.overflowWrap = 'anywhere';
      const name = document.createElement('strong');
      name.textContent = node.Name || node.ID;
      const address = document.createElement('div');
      address.textContent = node.URL || '尚未设置访问地址';
      identity.append(name, address);
      const status = document.createElement('span');
      status.dataset.sync = '';
      const edit = document.createElement('button');
      edit.type = 'button';
      edit.className = 'btn btn-secondary btn-sm';
      edit.textContent = local ? '本机设置' : '编辑连接';
      edit.onclick = () => open(node, row);
      summary.append(identity, status, edit);
      row.append(summary);
      members.append(row);
      if (local) byId('localMember').textContent = '当前登录节点 · ' + name.textContent;
    }
    syncStatus(data);
  }

  function close() {
    editor.hidden = true;
    token.value = '';
  }

  function open(node, row) {
    if (busy) return;
    selected = node;
    removing = false;
    const local = node?.ID === settings.Cluster.LocalID;
    (row || add.parentElement).after(editor);
    editor.hidden = false;
    byId('editorTitle').textContent = node ? '编辑 ' + (node.Name || node.ID) : '添加关联节点';
    byId('nodeIdentity').textContent = node?.ID || '保存时自动识别名称和 ID';
    byId('nameField').hidden = !local;
    byId('localName').value = local ? node.Name : '';
    byId('localName').placeholder = settings.DefaultNodeName;
    byId('nodeURL').value = node?.URL || (local ? location.href.split('?')[0].replace(/\/cluster\/?$/, '') : '');
    token.type = 'password';
    token.value = local ? settings.Cluster.Token : '';
    token.required = !node;
    token.minLength = 32;
    token.placeholder = node ? '留空保留当前令牌' : '';
    byId('tokenLabel').textContent = local ? '本机入站令牌' : '目标节点访问令牌';
    byId('tokenHelp').textContent = local ? '保存后本机启用新令牌，并向其他成员同步。' : '填写目标节点当前使用的入站令牌。';
    byId('generate').hidden = !local;
    byId('showToken').textContent = '显示令牌';
    remove.hidden = !node || local;
    remove.textContent = '移除节点…';
    byId('removePrompt').hidden = true;
    byId('nodeURL').focus();
  }

  async function save(change, auditChanged = false) {
    if (busy) return;
    busy = true;
    const buttons = [add, saveAudit, ...editor.querySelectorAll('button')];
    buttons.forEach(button => button.disabled = true);
    try {
      const current = await api('./cluster/settings');
      change(current);
      await api('./cluster/settings', {Cluster:current.Cluster, Audit:current.Audit});
      close();
      showStatus('本机已保存，正在同步到其他成员。' +
        (auditChanged ? '审计设置在各节点重启后生效。' : ''));
      const saved = await api('./cluster/settings');
      render(saved);
      resetAudit();
    } catch (error) {
      fail(error);
    } finally {
      busy = false;
      buttons.forEach(button => button.disabled = false);
    }
  }

  add.onclick = () => open(null);
  byId('cancelMember').onclick = close;
  byId('showToken').onclick = event => {
    token.type = token.type === 'password' ? 'text' : 'password';
    event.currentTarget.textContent = token.type === 'password' ? '显示令牌' : '隐藏令牌';
  };
  byId('generate').onclick = async event => {
    const button = event.currentTarget;
    const node = selected;
    button.disabled = true;
    try {
      const result = await api('./cluster/token', {});
      if (!editor.hidden && selected === node) token.value = result.Token;
    } catch (error) {
      fail(error);
    } finally {
      button.disabled = false;
    }
  };
  editor.onsubmit = event => {
    event.preventDefault();
    const url = byId('nodeURL').value.trim();
    if (!/^https?:\/\//i.test(url)) {
      showStatus('请输入 HTTP 或 HTTPS 访问地址。', true);
      return;
    }
    const name = byId('localName').value.trim();
    const credential = token.value.trim();
    save(({Cluster}) => {
      const node = Cluster.Nodes.find(n => n.ID === selected?.ID);
      if (selected && !node) throw new Error('节点配置已改变，请刷新页面。');
      if (!node) {
        Cluster.Nodes.push({URL:url, Token:credential});
        return;
      }
      node.URL = url;
      if (credential) node.Token = credential;
      if (node.ID === Cluster.LocalID) {
        node.Name = name;
        if (credential) Cluster.Token = credential;
      }
    });
  };
  remove.onclick = () => {
    if (!removing) {
      removing = true;
      const prompt = byId('removePrompt');
      prompt.textContent = '移除 ' + (selected.Name || selected.ID) + ' 并同步到其他成员？';
      prompt.hidden = false;
      remove.textContent = '确认移除并同步';
      return;
    }
    save(({Cluster}) => {
      Cluster.Nodes = Cluster.Nodes.filter(node => node.ID !== selected.ID);
    });
  };

  function resetAudit() {
    byId('driver').value = settings.Audit.Driver || 'sqlite';
    byId('dsn').value = settings.Audit.DSN || '';
  }
  byId('resetAudit').onclick = resetAudit;
  byId('auditSettings').onsubmit = event => {
    event.preventDefault();
    const Driver = byId('driver').value.trim();
    const DSN = byId('dsn').value.trim();
    save(data => { data.Audit = {Driver, DSN}; }, true);
  };

  api('./cluster/settings').then(data => {
    render(data);
    resetAudit();
    add.disabled = saveAudit.disabled = false;
    setInterval(async () => {
      if (busy) return;
      try {
        const current = await api('./cluster/settings');
        if (!busy) syncStatus(current);
      } catch (error) {
        if (!busy) fail(error);
      }
    }, 2500);
  }).catch(fail);
})();
