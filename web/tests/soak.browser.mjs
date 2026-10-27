// Long-session soak [SL-P0-6]: host + 2 guests (Chromium fake devices, playback
// muted) record one continuous take against the real daemon while resource use
// is sampled. Asserts no unbounded growth after warm-up (page JS heap after GC,
// DOM nodes/listeners, daemon RSS, open descriptors, goroutines, SFU/hub
// objects) and no growing upload backlog; then stops, asserts every track
// assembles, exports, and the export decodes with the take's duration (±2 s).
// Env: STUDIO_BINARY, SOAK_MINUTES (default 3), SOAK_SAMPLE_SEC (default 30),
// SOAK_OUT_DIR (CSV + summary; default a temp dir), STUDIO_TEST_PORT.
import assert from 'node:assert/strict';
import {spawn,execFileSync} from 'node:child_process';
import {mkdtemp,rm,readdir,mkdir,writeFile} from 'node:fs/promises';
import http from 'node:http';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {freePort,isolatedNetworkArgs,startTake,launchURL,readyToJoin} from './lib/harness.mjs';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');

const minutes=Number(process.env.SOAK_MINUTES||3),sampleSec=Number(process.env.SOAK_SAMPLE_SEC||30);
const warmupSec=Math.min(90,minutes*60/3);
const data=await mkdtemp(path.join(tmpdir(),'studio-soak-'));
const outDir=process.env.SOAK_OUT_DIR||await mkdtemp(path.join(tmpdir(),'studio-soak-out-'));await mkdir(outDir,{recursive:true});
const port=Number(process.env.STUDIO_TEST_PORT||await freePort('tcp')),origin=`http://127.0.0.1:${port}`;
const daemon=spawn(process.env.STUDIO_BINARY||'../bin/p2pvideod',['--data-dir',data,'--port',String(port),'--no-funnel',...await isolatedNetworkArgs()],{stdio:['ignore','pipe','pipe']});
let logs='';const keep=b=>{logs+=b;if(logs.length>200000)logs=logs.slice(-100000);};daemon.stderr.on('data',keep);daemon.stdout.on('data',keep);
const wait=ms=>new Promise(r=>setTimeout(r,ms));
const errors=[];let browser;
const names=['Host','Guest A','Guest B'];const pages=new Map(),cdp=new Map();

// Long postprocess requests outlive fetch's default header timeout.
const post=(url,headers)=>new Promise((resolve,reject)=>{const req=http.request(url,{method:'POST',headers},res=>{let body='';res.on('data',c=>body+=c);res.on('end',()=>resolve({status:res.statusCode,body}));});req.setTimeout(0);req.on('error',reject);req.end();});
const daemonProc=()=>{
 const rssKB=Number(execFileSync('ps',['-o','rss=','-p',String(daemon.pid)],{encoding:'utf8'}).trim());
 let fds=-1;try{fds=execFileSync('lsof',['-n','-P','-p',String(daemon.pid)],{encoding:'utf8',stdio:['ignore','pipe','ignore']}).trim().split('\n').length-1;}catch{}
 return {rssMB:rssKB/1024,lsofFDs:fds};
};
async function pageSample(name){
 const page=pages.get(name),session=cdp.get(name);
 await session.send('HeapProfiler.collectGarbage');
 const dom=await session.send('Memory.getDOMCounters');
 const m=await page.evaluate(async()=>{
  const est=await navigator.storage.estimate();
  const t=document.querySelector('#telemetry-chunks')?.textContent||'';
  const q=/([\d.]+) MB saved on the host, ([\d.]+) MB waiting/.exec(t);
  return {heapMB:performance.memory.usedJSHeapSize/1048576,opfsMB:(est.usage||0)/1048576,ackMB:q?Number(q[1]):NaN,queuedMB:q?Number(q[2]):NaN};
 });
 return {...m,domNodes:dom.nodes,listeners:dom.jsEventListeners};
}
const samples=[];
async function sample(t){
 const row={t:Math.round(t)};
 for(const name of names){const s=await pageSample(name);for(const [k,v] of Object.entries(s))row[`${name.replace(' ','')}_${k}`]=Math.round(v*10)/10;}
 const d=daemonProc();row.daemon_rssMB=Math.round(d.rssMB*10)/10;row.daemon_lsofFDs=d.lsofFDs;
 const rt=await(await fetch(origin+'/api/v1/debug/runtime')).json();
 row.daemon_goroutines=rt.goroutines;row.daemon_goHeapMB=Math.round(rt.heapAllocBytes/1048576*10)/10;row.daemon_openFDs=rt.openFDs;
 for(const [k,v] of Object.entries(rt.counts||{}))row[k]=v;
 samples.push(row);console.log(JSON.stringify(row));
}
// Growth from the first post-warm-up sample to the mean of the last two.
function growth(key){
 const post=samples.filter(s=>s.t>=warmupSec);assert.ok(post.length>=2,`not enough post-warm-up samples for ${key}`);
 const tail=post.slice(-2).map(s=>s[key]);return {base:post[0][key],end:(tail[0]+tail[1])/2,max:Math.max(...post.map(s=>s[key]))};
}
function slopePerMin(key){
 const post=samples.filter(s=>s.t>=warmupSec);const n=post.length,xs=post.map(s=>s.t/60),ys=post.map(s=>s[key]);
 const mx=xs.reduce((a,b)=>a+b)/n,my=ys.reduce((a,b)=>a+b)/n;
 const den=xs.reduce((a,x)=>a+(x-mx)**2,0);return den?xs.reduce((a,x,i)=>a+(x-mx)*(ys[i]-my),0)/den:0;
}

try{
 let ready=false;for(let i=0;i<100&&!ready;i++){try{ready=(await fetch(origin+'/health')).ok}catch{}if(!ready)await wait(100);}assert.ok(ready,'daemon did not start');
 browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH,args:['--use-fake-device-for-media-stream','--use-fake-ui-for-media-stream','--autoplay-policy=no-user-gesture-required','--mute-audio','--enable-precise-memory-info']});
 const open=async name=>{
  const context=await browser.newContext({permissions:['camera','microphone'],viewport:{width:1280,height:720}});
  const page=await context.newPage();page.setDefaultTimeout(45000);
  page.on('console',m=>{if(m.type()==='error'&&!/\b422\b/.test(m.text()))errors.push(`${name} console: ${m.text()}`);});
  page.on('pageerror',e=>errors.push(`${name}: ${e.message}`));
  pages.set(name,page);cdp.set(name,await context.newCDPSession(page));return page;
 };
 const host=await open('Host');
 await host.goto(await launchURL(data,origin));await readyToJoin(host);
 await host.locator('#name-input').fill('Host');
 const created=host.waitForResponse(r=>r.url().endsWith('/api/v1/sessions')&&r.request().method()==='POST');
 await host.locator('#join-btn').click();const session=await(await created).json();await host.locator('#studio-view').waitFor({state:'visible'});
 for(const name of names.slice(1)){
  const guest=await open(name);
  await guest.goto(`${origin}/join?session=${session.id}&token=${session.token}`);await guest.waitForFunction(()=>document.querySelector('#preview-video').videoWidth>0);
  await guest.locator('#name-input').fill(name);await guest.locator('#join-btn').click();
  await host.waitForFunction(name=>document.querySelector('#host-admission-banner').style.display==='flex'&&document.querySelector('#admission-guest-name').textContent===name,name);
  await host.locator('#admit-btn').click();await guest.locator('#studio-view').waitFor({state:'visible'});
 }
 for(const page of pages.values())await page.waitForFunction(()=>document.querySelectorAll('.tile[data-participant-id] video').length===2&&[...document.querySelectorAll('.tile[data-participant-id] video')].every(v=>v.videoWidth>0),{},{timeout:30000});
 console.log(`3 participants decode each other; recording ${minutes} min, sampling every ${sampleSec}s`);
 const confirmed=await startTake(host);if(confirmed)console.log('pre-flight confirm:',JSON.stringify(confirmed));
 // Headless contexts only lack quota for a 2 h take; devices and paths must read healthy.
 for(const line of confirmed.split('\n').filter(Boolean))assert.match(line,/· Storage: /,`unexpected red pre-flight item: ${line}`);
 for(const page of pages.values())await page.waitForFunction(()=>document.querySelector('#rec-indicator').style.display==='inline-flex');
 const startedAt=Date.now();
 await sample(0);
 while(Date.now()-startedAt<minutes*60000){
  const next=Math.min(minutes*60000,(samples.length)*sampleSec*1000);
  await wait(Math.max(0,startedAt+next-Date.now()));
  await sample((Date.now()-startedAt)/1000);
  // Media keeps flowing on every page for the whole take.
  for(const [name,page] of pages){const live=await page.evaluate(()=>document.querySelector('#conn-text').textContent);assert.equal(live,'Remote video decoded',`${name} lost call media at ${Math.round((Date.now()-startedAt)/1000)}s`);}
 }
 await host.locator('#rec-btn').click();const stoppedAt=Date.now();
 const header=Object.keys(samples[0]);
 await writeFile(path.join(outDir,'soak-samples.csv'),[header.join(','),...samples.map(s=>header.map(k=>s[k]).join(','))].join('\n')+'\n');

 // No unbounded growth after warm-up.
 const report={minutes,sampleSec,warmupSec,samples:samples.length,checks:{}};
 const check=(key,{absolute,relative=0,slope})=>{const g=growth(key),s=slopePerMin(key),allowed=Math.max(absolute,g.base*relative);report.checks[key]={...g,growth:Math.round((g.end-g.base)*10)/10,allowed,slopePerMin:Math.round(s*100)/100};
  assert.ok(g.end-g.base<=allowed,`${key} grew ${g.base} → ${g.end} (allowed +${allowed})`);
  if(slope!==undefined&&minutes>=10)assert.ok(s<=slope,`${key} trend ${s.toFixed(2)}/min exceeds ${slope}/min`);};
 for(const name of names){const p=name.replace(' ','');
  check(`${p}_heapMB`,{absolute:16,relative:0.25,slope:0.5});
  check(`${p}_domNodes`,{absolute:300,slope:20});
  check(`${p}_listeners`,{absolute:100,slope:5});
  const q=samples.filter(s=>s.t>=warmupSec).map(s=>s[`${p}_queuedMB`]);
  assert.ok(q.every(Number.isFinite),`${p} upload telemetry missing`);
  report.checks[`${p}_queuedMB`]={max:Math.max(...q),last:q.at(-1)};
  assert.ok(Math.max(...q)<=32,`${p} upload backlog reached ${Math.max(...q)} MB`);
  assert.ok(q.at(-1)<=Math.max(8,q[0]*2),`${p} upload backlog growing: ${q.join(',')}`);
 }
 check('daemon_rssMB',{absolute:64,relative:0.3,slope:2});
 check('daemon_goHeapMB',{absolute:32,relative:0.5,slope:1});
 check('daemon_lsofFDs',{absolute:16,slope:0.5});
 check('daemon_goroutines',{absolute:24,slope:1});
 for(const k of ['sfuPeers','sfuPublications','sfuSubscriptions','hubClients'])check(k,{absolute:0});

 // Every track assembles; the export decodes with the take's duration.
 const headers={Authorization:`Bearer ${session.hostToken}`};let snapshot;
 for(let i=0;i<1200;i++){const d=await(await fetch(`${origin}/api/v1/sessions/${session.id}`,{headers})).json();const take=d.takes?.[0];if(take){snapshot=await(await fetch(`${origin}/api/v1/takes/${take.id}`,{headers})).json();if(snapshot.tracks.length===3&&snapshot.tracks.every(t=>t.status==='assembled'))break;}await wait(500);}
 assert.equal(snapshot.tracks.length,3,JSON.stringify(snapshot));assert.ok(snapshot.tracks.every(t=>t.status==='assembled'&&t.totalBytes>1000),JSON.stringify(snapshot.tracks));
 const assembledAfterStopSec=(Date.now()-stoppedAt)/1000;
 const takeSec=(snapshot.take.stopAtHostUs-snapshot.take.startAtHostUs)/1e6;
 assert.ok(Math.abs(takeSec-(stoppedAt-startedAt)/1000)<5,`take ${takeSec}s vs wall ${(stoppedAt-startedAt)/1000}s`);
 const t0=Date.now();const pp=await post(`${origin}/api/v1/takes/${snapshot.take.id}/postprocess`,headers);assert.equal(pp.status,200,pp.body);const exportSec=(Date.now()-t0)/1000;
 const out=path.join(data,'sessions',session.id,'takes',snapshot.take.id,'out');
 const exported=[];
 for(const dir of ['video','audio'])for(const file of await readdir(path.join(out,dir))){
  const f=path.join(out,dir,file);
  const dur=Number(execFileSync('ffprobe',['-v','error','-show_entries','format=duration','-of','csv=p=0',f],{encoding:'utf8'}).trim());
  execFileSync('ffmpeg',['-v','error','-i',f,'-f','null','-'],{stdio:'pipe'});
  exported.push({file:`${dir}/${file}`,durationSec:Math.round(dur*100)/100});
  assert.ok(Math.abs(dur-takeSec)<=2,`${dir}/${file} lasts ${dur}s, take ${takeSec}s`);
 }
 assert.equal(exported.filter(e=>e.file.startsWith('video/')).length,3);assert.equal(exported.filter(e=>e.file.startsWith('audio/')).length,3);
 assert.deepEqual(errors,[]);
 Object.assign(report,{takeSec:Math.round(takeSec*100)/100,assembledTracks:snapshot.tracks.length,trackMB:snapshot.tracks.map(t=>Math.round(t.totalBytes/1048576)),assembledAfterStopSec:Math.round(assembledAfterStopSec*10)/10,exportSec:Math.round(exportSec),exported,errors});
 await writeFile(path.join(outDir,'soak-summary.json'),JSON.stringify(report,null,1));
 console.log(JSON.stringify(report));console.log('CSV:',path.join(outDir,'soak-samples.csv'));
}catch(e){
 if(samples.length){const header=Object.keys(samples[0]);await writeFile(path.join(outDir,'soak-samples.csv'),[header.join(','),...samples.map(s=>header.map(k=>s[k]).join(','))].join('\n')+'\n').catch(()=>{});}
 console.error('errors',JSON.stringify(errors));console.error(logs.slice(-6000));throw e;
}finally{
 await browser?.close();
 if(daemon.exitCode===null&&daemon.signalCode===null){daemon.kill('SIGTERM');await Promise.race([new Promise(r=>daemon.once('exit',r)),wait(3000)]);if(daemon.exitCode===null&&daemon.signalCode===null)daemon.kill('SIGKILL');}
 await rm(data,{recursive:true,force:true});
}
