import test from 'node:test';
import assert from 'node:assert/strict';
import * as idb from '../static/js/core/idb.js';
import * as sources from '../static/js/core/sources.js';
import { afterLogin, saveSubscriptionToAccount } from '../static/js/core/keyring.js';

// 只模拟本次操作用到的 DOM 接口，运行真实的设置页事件处理代码。
class Element {
  constructor() { this.value=''; this.textContent=''; this.disabled=false; this.events={}; this.children=[]; this.attributes={}; this.dataset={}; const classes=new Set(); this.classList={add:x=>classes.add(x),remove:x=>classes.delete(x),contains:x=>classes.has(x)}; }
  addEventListener(name, fn) { this.events[name]=fn; }
  setAttribute(name, value) { this.attributes[name]=value; }
  replaceChildren() { this.children=[]; }
  append(...items) { this.children.push(...items); }
  querySelector() { return null; }
  querySelectorAll() { return []; }
}
let nodes,remote,mode,started,release,passwordPosts;
const OriginalFormData=globalThis.FormData;
const oldURL='https://moovie.test/old.json',newURL='https://moovie.test/new.json';

test.beforeEach(async t=>{
  await idb.clearPrefix('u1:'); remote=null; mode='ok'; started=false; release=null;passwordPosts=0;
  delete globalThis.isSecureContext;
  nodes=Object.fromEntries(['sub-form','sub-url','sub-status','src-table','sub-refresh','sub-clear'].map(k=>[k,new Element()]));
  nodes['sub-form'].buttons=[new Element(),nodes['sub-refresh'],nodes['sub-clear']];
  nodes['sub-form'].querySelectorAll=()=>nodes['sub-form'].buttons;
  nodes['password-form']=new Element(); nodes['password-button']=new Element();
  nodes['current']=new Element();nodes['current'].value='password-123';
  nodes['password']=new Element();nodes['password'].value='new-password-456';
  nodes['password-form'].action='/settings/password';
  nodes['password-form'].querySelector=selector=>selector==='button'?nodes['password-button']:selector.includes('current')?nodes['current']:selector.includes('name=')?nodes['password']:nodes['password-form'].children.find(x=>Object.hasOwn(x.attributes,'data-password-status'));
  globalThis.FormData=class {constructor(){this.values=new Map([['current',nodes['current'].value],['password',nodes['password'].value]])}set(k,v){this.values.set(k,v)}get(k){return this.values.get(k)}};
  t.after(()=>{globalThis.FormData=OriginalFormData;delete globalThis.isSecureContext});
  globalThis.document={body:{dataset:{user:'1'}},querySelector:selector=>selector.includes('csrf-token')?{content:'csrf'}:selector.includes('/settings/password')?nodes['password-form']:null,getElementById:id=>nodes[id]??null,createElement:()=>new Element(),createTextNode:text=>text};
  globalThis.location=new URL('https://moovie.test/settings');
  globalThis.confirm=()=>true;
  globalThis.CSS={escape:value=>value};
  globalThis.fetch=async(path,options={})=>{
    const u=new URL(path,location.href);
    if(u.pathname==='/api/keyring'){
      if(options.method==='PUT'){
        if(mode==='hold'){started=true;await new Promise(resolve=>release=resolve)}
        if(mode==='fail')return Response.json({error:'failure'},{status:500});
        const body=JSON.parse(options.body);
        remote={...body,version:(remote?.version??0)+1};
        return Response.json({version:remote.version});
      }
      return remote?Response.json(remote):Response.json({error:'none'},{status:404});
    }
    if(u.pathname==='/settings/password'){
      passwordPosts++;
      if(mode==='fail')return Response.json({error:'密码修改失败，请重试。'},{status:500});
      const wire=JSON.parse(options.body.get('keyring'));
      assert.equal(wire.base,remote.version);
      remote={...wire,version:remote.version+1};
      return Response.json({keyring_version:remote.version,redirect:'https://moovie.test/settings?saved=account#account'});
    }
    if(u.pathname.endsWith('.json'))return Response.json({sources:[{key:'test',name:'测试源',api:'https://source.example/api.php/provide/vod/'}]});
    if(u.pathname==='/api/relay')return Response.json({list:[]});
    throw new Error('unexpected request');
  };
  await afterLogin('password-123');
  await saveSubscriptionToAccount(oldURL);
  await import('../static/js/pages/settings.js?case='+t.name);
  await new Promise(resolve=>setTimeout(resolve,0));
});

test('设置页只在服务器确认后提示已保存到账号',async()=>{
  nodes['sub-url'].value=newURL;mode='hold';
  const saving=nodes['sub-form'].events.submit({preventDefault(){}});
  for(let i=0;i<200&&!started;i++)await new Promise(resolve=>setTimeout(resolve,2));
  assert.ok(started);
  assert.equal(nodes['sub-form'].buttons[0].disabled,true);
  assert.ok(!nodes['sub-status'].textContent.includes('已保存到账号'));
  assert.equal((await sources.getSubscription()).url,oldURL);
  release();await saving;
  assert.ok(nodes['sub-status'].textContent.includes('已保存到账号'));
  assert.equal(nodes['sub-form'].buttons[0].disabled,false);
  assert.equal((await sources.getSubscription()).url,newURL);
});

test('设置页上传失败有持续可见的失败提示，并保留旧配置和待重试输入',async()=>{
  nodes['sub-url'].value=newURL;mode='fail';
  await nodes['sub-form'].events.submit({preventDefault(){}});
  assert.equal(nodes['sub-status'].classList.contains('error'),true);
  assert.equal(nodes['sub-status'].attributes.role,'alert');
  assert.match(nodes['sub-status'].textContent,/服务器/);
  assert.equal(nodes['sub-url'].value,newURL);
  assert.equal((await sources.getSubscription()).url,oldURL);
  assert.equal(nodes['sub-form'].buttons[0].disabled,false);
});

test('清除失败时保留旧订阅并显示错误',async()=>{
  mode='fail';
  await nodes['sub-clear'].events.click();
  assert.match(nodes['sub-status'].textContent,/服务器/);
  assert.equal(nodes['sub-status'].attributes.role,'alert');
  assert.equal((await sources.getSubscription()).url,oldURL);
});


test('HTTP 加密不可用时不发送改密码请求，并在密码表单显示失败原因',async()=>{
  globalThis.isSecureContext=false;
  await nodes['password-form'].events.submit({preventDefault(){},currentTarget:nodes['password-form']});
  assert.equal(passwordPosts,0);
  const notice=nodes['password-form'].children[0];
  assert.match(notice.textContent,/HTTPS/);
  assert.equal(notice.attributes.role,'alert');
  assert.equal(nodes['password-button'].disabled,false);
});

test('改密码服务端失败会显示错误并保留原账号配置',async()=>{
  const originalBlob=remote.blob;mode='fail';
  await nodes['password-form'].events.submit({preventDefault(){},currentTarget:nodes['password-form']});
  assert.equal(passwordPosts,1);
  assert.equal(remote.blob,originalBlob);
  assert.match(nodes['password-form'].children[0].textContent,/密码修改失败/);
  assert.equal(nodes['password-form'].children[0].attributes.role,'alert');
});

test('改密码提交包含预先加密的配置，确认后才切换本机密钥并跳转',async()=>{
  const previous=await idb.get('kv','u1:keyring');
  await nodes['password-form'].events.submit({preventDefault(){},currentTarget:nodes['password-form']});
  assert.equal(passwordPosts,1);
  const local=await idb.get('kv','u1:keyring');
  assert.notEqual(local.salt,previous.salt);
  assert.equal(local.version,remote.version);
  assert.equal(location.search,'?saved=account');
  assert.equal((await sources.getSubscription()).url,oldURL);
});
