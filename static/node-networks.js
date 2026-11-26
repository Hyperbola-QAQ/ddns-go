// Reuse the original acquisition forms, grouped by protocol and then by node.
const interfaceCache = new Map();
function legacyNetwork(conf, id) {
  const n = {NodeID: id, Suffixes: ''};
  for (const family of ['Ipv4', 'Ipv6']) {
    n[family] = {};
    for (const key of ['Enable', 'GetType', 'Url', 'NetInterface', 'Cmd', 'Reg', 'Domains']) {
      const value = conf[family + key];
      n[family][key === 'Url' ? 'URL' : key === 'Reg' ? 'Ipv6Reg' : key] = key === 'Domains' ? (value || '').split(/\r?\n/).map(s=>s.trim()).filter(Boolean) : value ?? '';
    }
  }
  return n;
}
async function loadInterfaces(id, refresh = false) {
  if (!refresh && interfaceCache.has(id)) return interfaceCache.get(id);
  const response = await fetch('./cluster/interfaces?node=' + encodeURIComponent(id));
  if (!response.ok) throw new Error(await response.text());
  const data = await response.json();interfaceCache.set(id, data);return data;
}
function lifetimeText(life) {
  if (!life?.Known) return '有效期未知';
  if (life.ValidSeconds === 4294967295) return '永久有效';
  return '有效期 ' + life.ValidSeconds + ' 秒';
}
function renderNodeNetworks(conf) {
  conf.Cluster ||= {};
  for (const [id,key,fallback] of [['prefixCooperation','Enabled',false],['prefixLength','PrefixLength',64],['prefixCooldown','CooldownSeconds',60]]) {
    const input=document.getElementById(id);
    if(input.type==='checkbox')input.checked=!!conf.Cluster[key];else input.value=conf.Cluster[key]||fallback;
    input.onchange=()=>conf.Cluster[key]=input.type==='checkbox'?input.checked:Number(input.value);
  }
  if (!conf.Networks?.length) conf.Networks = [legacyNetwork(conf, localNodeID)];
  conf.Networks = conf.Networks.filter(n=>clusterNodes.some(node=>node.ID===n.NodeID));
  for (const node of clusterNodes) if (!conf.Networks.some(n=>n.NodeID===node.ID)) {
    const n=legacyNetwork(defaultDnsConf,node.ID);
    n.Ipv4.Enable=false;n.Ipv6.Enable=false;n.Ipv4.Domains=[];n.Ipv6.Domains=[];conf.Networks.push(n);
  }
  conf.Cluster.NodeIDs=conf.Networks.map(n=>n.NodeID).join(',');
  renderSuffixGroups(conf);
  for (const family of ['Ipv4','Ipv6']) {
    const container=document.getElementById('nodeNetworks'+family.slice(-1));container.replaceChildren();
    conf.Networks.forEach((network,index)=>{
      const source=document.getElementById('networkTemplate').content.cloneNode(true);
      const card=Array.from(source.querySelectorAll('.portlet')).find(el=>el.querySelector('h5')?.textContent===family.replace('Ipv','IPv'));
      const values=network[family] ||= {};
      const title=card.querySelector('h5');title.textContent=(clusterNodes.find(n=>n.ID===network.NodeID)?.Name||network.NodeID)+' ('+network.NodeID+')';
      const prefix='node-'+index+'-';
      card.querySelectorAll('[id]').forEach(el=>el.id=prefix+el.id);
      card.querySelectorAll('[for]').forEach(el=>el.htmlFor=prefix+el.htmlFor);
      card.querySelectorAll('[aria-describedby]').forEach(el=>el.setAttribute('aria-describedby',prefix+el.getAttribute('aria-describedby')));
      const body=card.querySelector('.portlet__body');
      function visibility(){
        body.querySelectorAll('.form-group').forEach(row=>row.hidden=!values.Enable&&!row.querySelector('input[type=checkbox]'));
        body.querySelectorAll('[data-visible]').forEach(el=>el.style.display=el.dataset.visible===values.GetType?'':'none');
      }
      let updateAddress=()=>{};
      card.querySelectorAll('[name]').forEach(input=>{
        const keyName=input.name.slice(4),key=keyName==='Url'?'URL':keyName==='Reg'?'Ipv6Reg':keyName;
        input.name=prefix+input.name;
        if(input.type==='checkbox')input.checked=!!values[key];
        else if(input.type==='radio')input.checked=input.value===values[key];
        else if(input.tagName!=='SELECT')input.value=key==='Domains'?(values[key]||[]).join('\n'):values[key]||'';
        input.addEventListener(input.type==='checkbox'||input.type==='radio'||input.tagName==='SELECT'?'change':'input',()=>{
          values[key]=input.type==='checkbox'?input.checked:key==='Domains'?input.value.split(/\r?\n/).map(s=>s.trim()).filter(Boolean):input.value;
          if(key==='NetInterface')updateAddress();visibility();
        });
      });
      const select=card.querySelector('select[name$=NetInterface]');select.replaceChildren();
      const saved=document.createElement('option');saved.value=values.NetInterface||'';saved.textContent=values.NetInterface||'正在读取目标节点网卡…';select.append(saved);
      const status=document.createElement('small');status.className='form-text text-muted';status.dataset.visible='netInterface';select.after(status);
      const refresh=document.createElement('button');refresh.type='button';refresh.className='btn btn-secondary btn-sm mt-2';refresh.textContent='刷新网卡';refresh.dataset.visible='netInterface';status.after(refresh);
      let addressSelect;
      if(family==='Ipv6') {
        addressSelect=document.createElement('select');addressSelect.className='form-control mt-2';addressSelect.setAttribute('aria-label','IPv6 地址选择');addressSelect.dataset.visible='netInterface';refresh.after(addressSelect);
        addressSelect.onchange=()=>{if(addressSelect.value!=='custom'){values.Ipv6Reg=addressSelect.value;card.querySelector('input[name$=Reg]').value=values.Ipv6Reg;}};
      }
      async function populate(refreshNow=false){
        try{
          const data=await loadInterfaces(network.NodeID,refreshNow);if(!card.isConnected)return;
          const list=data[family]||[];select.replaceChildren();
          for(const item of list){const option=document.createElement('option');option.value=item.Name;option.textContent=item.Name+' — '+item.Address.join(', ');select.append(option);}
          if(values.NetInterface&&!list.some(n=>n.Name===values.NetInterface)){const option=document.createElement('option');option.value=values.NetInterface;option.textContent=values.NetInterface+'（当前不可用，保留配置）';select.append(option);}
          if(!values.NetInterface&&list.length)values.NetInterface=list[0].Name;
          select.value=values.NetInterface||'';status.textContent=list.length?'来自节点 '+network.NodeID+(family==='Ipv6'?'；默认选择有效期最长的地址':''):'该节点暂无可用地址';
          updateAddress=()=>{
            if(!addressSelect)return;addressSelect.replaceChildren();
            const add=(value,text)=>{const option=document.createElement('option');option.value=value;option.textContent=text;addressSelect.append(option);};
            add('','自动选择有效期最长的 IPv6 地址');
            const iface=list.find(n=>n.Name===values.NetInterface);
            (iface?.Address||[]).forEach((ip,i)=>add('@'+(i+1),ip+'（'+lifetimeText(iface.Lifetimes?.[i])+'）'));
            add('custom','使用下方自定义正则表达式');addressSelect.value=Array.from(addressSelect.options).some(o=>o.value===values.Ipv6Reg)?values.Ipv6Reg:'custom';
          };updateAddress();visibility();
        }catch(e){if(card.isConnected)status.textContent=e.message;}
      }
      refresh.onclick=()=>populate(true);convertDom(card);container.append(card);visibility();populate();
    });
  }
}

// Parse the canonical IPv6 spelling into eight words, including compressed zeros.
function ipv6Value(address) {
  if (!/^[0-9a-f:]+$/i.test(address) || !address.includes(':')) throw Error();
  const canonical = new URL('http://[' + address + ']/').hostname.slice(1, -1);
  const [left, right] = canonical.split('::');
  const leading = left ? left.split(':') : [];
  const trailing = right ? right.split(':') : [];
  const words = [...leading, ...Array(8-leading.length-trailing.length).fill('0'), ...trailing];
  return BigInt('0x' + words.map(word => word.padStart(4, '0')).join(''));
}

function renderSuffixGroups(conf) {
  const snapshot = prefixSnapshots?.[conf.ID];
  const prefix = snapshot?.Prefix || '';
  document.getElementById('confirmedPrefix').textContent = prefix || '等待首次成功更新';
  const reports = document.getElementById('prefixReports');
  reports.replaceChildren();
  const participants = conf.Networks.filter(n => n.Ipv6?.Enable);
  let matched = 0;
  for (const network of participants) {
    const item = document.createElement('li');
    const node = clusterNodes.find(n => n.ID === network.NodeID);
    const address = snapshot?.Reports?.[network.NodeID];
    if (prefix && address) {
      const [networkAddress, bits] = prefix.split('/');
      const hostBits = BigInt(128 - Number(bits));
      if ((ipv6Value(address) >> hostBits) === (ipv6Value(networkAddress) >> hostBits)) matched++;
    }
    item.textContent = [node?.Name || network.NodeID, address || '暂无上报', ...(network.Ipv6.Domains || [])].join(' · ');
    reports.append(item);
  }

  document.getElementById('prefixAgreement').textContent = prefix && participants.length
    ? matched + ' / ' + participants.length + ' 个节点最近上报与已确认前缀一致' : '';

  const config = conf.IPv6Suffix ||= {Enable:false, Groups:[]};
  config.Groups ||= [];
  const enabled = document.getElementById('suffixEnabled');
  const body = document.getElementById('suffixBody');
  const list = document.getElementById('suffixGroups');
  enabled.checked = config.Enable;
  body.hidden = !config.Enable;
  list.replaceChildren();

  function showGroup(group, open) {
    const row = document.getElementById('suffixGroupTemplate').content.firstElementChild.cloneNode(true);
    const suffix = row.querySelector('input');
    const domains = row.querySelector('textarea');
    const preview = row.querySelector('output');
    const summary = row.querySelector('summary');
    function updatePreview() {
      const [network, bits] = prefix.split('/');
      if (!network || Number(bits) !== Number(conf.Cluster.PrefixLength)) {
        preview.textContent = '等待确认当前长度的公共前缀';
        return;
      }
      try {
        const host = ipv6Value(group.Suffix);
        if (host >= (1n << BigInt(128-Number(bits)))) throw Error();
        const hex = (ipv6Value(network) | host).toString(16).padStart(32,'0');
        preview.textContent = 'AAAA → ' + new URL('http://['+hex.match(/.{4}/g).join(':')+']/').hostname.slice(1,-1);
      } catch {
        preview.textContent = '请输入仅含主机位的 IPv6 后缀';
      }
    }
    suffix.value = group.Suffix;
    suffix.oninput = () => { group.Suffix = suffix.value.trim(); updatePreview(); };
    domains.value = (group.Domains || []).join('\n');
    domains.oninput = () => {
      group.Domains = domains.value.split(/\r?\n/).map(s => s.trim()).filter(Boolean);
      summary.textContent = '关联域名（' + group.Domains.length + '）';
    };
    domains.oninput();
    updatePreview();
    row.querySelector('details').open = open;
    row.querySelector('button').onclick = () => {config.Groups.splice(config.Groups.indexOf(group), 1); row.remove();};
    list.append(row);
    if (open) suffix.focus();
  }
  function add() {
    const group = {Suffix:'', Domains:[]};
    config.Groups.push(group);
    showGroup(group, true);
  }
  enabled.onchange = () => {
    config.Enable = enabled.checked;
    body.hidden = !enabled.checked;
    if (enabled.checked && !config.Groups.length) add();
  };
  document.getElementById('addSuffixGroup').onclick = add;
  config.Groups.forEach(group => showGroup(group, false));
  document.getElementById('prefixLength').onchange = event => {
    conf.Cluster.PrefixLength = Number(event.target.value);
    list.querySelectorAll('input').forEach(input => input.oninput());
  };
}
