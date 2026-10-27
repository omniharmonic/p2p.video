// Producer role [SL-P1-11] against the real daemon with Chromium fake
// devices: host + producer + guest. The host invites a producer with the
// separate producer link and admits them; the producer is heard (camera off
// by default), badged on tiles and in People, admits the waiting guest,
// starts and stops a take without recording itself; the take and the export
// contain host + guest only; the producer can't end the meeting or reach any
// host control. Build first; STUDIO_BINARY is the daemon.
import assert from 'node:assert/strict';
import {spawn} from 'node:child_process';
import {mkdtemp,readFile,mkdir} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {isolatedNetworkArgs,startTake,launchURL,revealControl,readyToJoin} from './lib/harness.mjs';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const data=await mkdtemp(path.join(tmpdir(),'studio-producer-'));
const port=Number(process.env.STUDIO_TEST_PORT||19072),origin=`http://127.0.0.1:${port}`;
const daemon=spawn(process.env.STUDIO_BINARY||'../bin/p2pvideod',['--data-dir',data,'--port',String(port),'--no-funnel',...await isolatedNetworkArgs()],{stdio:['ignore','pipe','pipe']});
let logs='';daemon.stderr.on('data',b=>logs+=b);daemon.stdout.on('data',b=>logs+=b);
const screenshotDir=process.env.STUDIO_SCREENSHOT_DIR;
if(screenshotDir)await mkdir(screenshotDir,{recursive:true});
const screenshot=async(name,page)=>{if(screenshotDir)await page.screenshot({path:path.join(screenshotDir,name+'.png'),animations:'disabled'});};
const errors=[];let browser;
const wait=ms=>new Promise(r=>setTimeout(r,ms));
const expectedConsole=m=>/422|Failed to load resource/.test(m.text());

try{
 let ready=false;for(let i=0;i<100;i++){try{ready=(await fetch(origin+'/health')).ok;if(ready)break;}catch{}await wait(100);}assert.ok(ready,'daemon did not start: '+logs);
 browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH,args:['--use-fake-device-for-media-stream','--use-fake-ui-for-media-stream','--autoplay-policy=no-user-gesture-required','--mute-audio']});
 const open=async name=>{
  const context=await browser.newContext({permissions:['camera','microphone','clipboard-read','clipboard-write'],viewport:{width:1280,height:800}});
  const page=await context.newPage();page.setDefaultTimeout(45000);
  page.on('console',m=>{if(m.type()==='error'&&!expectedConsole(m))errors.push(`${name} console: ${m.text()}`);});
  page.on('pageerror',e=>errors.push(`${name}: ${e.message}`));
  return page;
 };
 const host=await open('Host');
 await host.goto(await launchURL(data,origin));await readyToJoin(host);
 await host.locator('#name-input').fill('Host');
 const created=host.waitForResponse(r=>r.url().endsWith('/api/v1/sessions')&&r.request().method()==='POST');
 await host.locator('#join-btn').click();const session=await(await created).json();
 await host.locator('#studio-view').waitFor({state:'visible'});

 // The host invites a producer from More → Invite producer.
 await (await revealControl(host,'#producer-link-btn')).click();
 await host.locator('#producer-dialog').waitFor({state:'visible'});
 const producerURL=await host.locator('#producer-link-value').inputValue();
 const producerPath=new URL(producerURL).pathname+new URL(producerURL).search;
 const ptoken=new URL(producerURL).searchParams.get('token');
 assert.ok(ptoken&&ptoken!==session.token,'a separate producer link');
 await screenshot('producer-01-invite',host);
 await host.locator('[aria-label="Close producer invitation"]').click();

 // Producer green room says so; joins and waits for the host.
 const producer=await open('Producer');
 await producer.goto(origin+producerPath);
 await producer.waitForFunction(()=>document.querySelector('#preview-video').videoWidth>0);
 await producer.locator('#producer-note').waitFor({state:'visible'});
 assert.equal(await producer.locator('#pcm-audio-toggle').isVisible(),false,'no recording options for a producer');
 await screenshot('producer-02-green-room',producer);
 await producer.locator('#name-input').fill('Pat');await producer.locator('#join-btn').click();
 await host.waitForFunction(()=>document.querySelector('#host-admission-banner').style.display==='flex'&&/producer/.test(document.querySelector('#admission-guest-name').textContent));
 await host.locator('#admit-btn').click();
 await producer.locator('#studio-view').waitFor({state:'visible'});
 await producer.waitForFunction(()=>/Producer/.test(document.querySelector('#local-name-tag').textContent));
 // Producer view: Record + source health; no host management.
 assert.equal(await producer.locator('#rec-btn').isVisible(),true,'producer can record');
 for(const id of ['#settings-btn','#copy-invite-btn','#chirp-btn','#postproc-btn','#producer-link-btn','#end-meeting-btn'])assert.equal(await producer.locator(id).isVisible().catch(()=>false),false,id+' shown to a producer');
 assert.equal(await producer.locator('#toggle-cam-btn').textContent(),'Enable camera','camera starts off; labels never mention recording');
 // The host sees the producer badged on its tile and in People.
 await host.waitForFunction(()=>document.querySelector('#video-grid .tile[data-participant-id] .tile-role-producer')?.textContent==='Producer');
 await host.locator('#people-btn').click();
 await host.waitForFunction(()=>[...document.querySelectorAll('.people-role-producer')].some(e=>/Producer · not recorded/.test(e.textContent)));
 await screenshot('producer-03-host-people',host);
 await host.locator('#people-btn').click();
 // The producer can be heard: the host receives its audio.
 const inboundAudio=()=>host.evaluate(async()=>{let t=0;for(const v of document.querySelectorAll('#video-grid .tile[data-participant-id] video')){const s=v.srcObject;if(s?.getAudioTracks().length)t++;}return t;});
 await host.waitForFunction(()=>[...document.querySelectorAll('#video-grid .tile[data-participant-id] video')].some(v=>v.srcObject?.getAudioTracks().some(t=>t.readyState==='live')));
 assert.ok(await inboundAudio()>=1);

 // A guest asks to join; the producer admits them.
 const guest=await open('Guest');
 await guest.goto(`${origin}/join?session=${session.id}&token=${session.token}`);
 await guest.waitForFunction(()=>document.querySelector('#preview-video').videoWidth>0);
 await guest.locator('#name-input').fill('Gia');await guest.locator('#join-btn').click();
 await producer.waitForFunction(()=>document.querySelector('#host-admission-banner').style.display==='flex'&&/Gia/.test(document.querySelector('#admission-guest-name').textContent));
 await screenshot('producer-04-admits-guest',producer);
 await producer.locator('#admit-btn').click();
 await guest.locator('#studio-view').waitFor({state:'visible'});
 console.log('producer admitted the guest');
 // Guest people list: producer shown, no controls for the guest.
 await guest.locator('#people-btn').click();
 await guest.waitForFunction(()=>[...document.querySelectorAll('.people-role-producer')].length===1&&document.querySelectorAll('.people-actions').length===0);
 await guest.locator('#people-btn').click();
 // Producer's People: Mute for guest and not the host; Remove only for the guest.
 await producer.locator('#people-btn').click();
 await producer.waitForFunction(()=>document.querySelectorAll('.people-row').length===3);
 const actions=await producer.evaluate(()=>Object.fromEntries([...document.querySelectorAll('.people-row')].map(r=>[r.querySelector('strong').textContent,[...r.querySelectorAll('[data-action]')].map(b=>b.dataset.action)])));
 assert.deepEqual(actions,{'Host':[],'Gia':['mute','remove'],'Pat (you)':[]},JSON.stringify(actions));
 await producer.locator('#people-btn').click();
 // Source health for the producer.
 await (await revealControl(producer,'#toggle-health-btn')).click();
 await producer.waitForFunction(()=>/Gia/.test(document.querySelector('#health-participants-list').textContent)&&/Producer · not recorded/.test(document.querySelector('#health-participants-list').textContent));

 // The producer starts a take, never records itself, and stops it.
 const problems=await startTake(producer);
 if(problems)console.log('pre-flight problems confirmed:',problems.replace(/\s+/g,' ').slice(0,200));
 assert.ok(!/Pat/.test(problems||''),'the producer never blocks a take it is not in');
 await guest.waitForFunction(()=>document.querySelector('#rec-indicator').style.display==='inline-flex');
 await host.waitForFunction(()=>document.querySelector('#rec-indicator').style.display==='inline-flex');
 await producer.waitForFunction(()=>/Stop/.test(document.querySelector('#rec-btn').textContent));
 assert.equal(await producer.locator('#local-iso-badge').isVisible(),false,'producer shows no local recording');
 await screenshot('producer-05-recording',producer);
 await wait(4000);
 await producer.locator('#rec-btn').click();
 await guest.waitForFunction(()=>document.querySelector('#rec-indicator').style.display==='none');
 await producer.waitForFunction(()=>/Start/.test(document.querySelector('#rec-btn').textContent));

 let snapshot;
 for(let i=0;i<120;i++){
  const detail=await(await fetch(`${origin}/api/v1/sessions/${session.id}`,{headers:{Authorization:`Bearer ${session.hostToken}`}})).json();
  const take=detail.takes?.[0];
  if(take){snapshot=await(await fetch(`${origin}/api/v1/takes/${take.id}`,{headers:{Authorization:`Bearer ${session.hostToken}`}})).json();if(snapshot.tracks.length>=2&&snapshot.tracks.every(t=>t.status==='assembled')&&!snapshot.missingParticipants.length)break;}
  await wait(500);
 }
 const names=[...new Set(snapshot.tracks.map(t=>t.participantName))].sort();
 assert.deepEqual(names,['Gia','Host'],'take tracks: '+JSON.stringify(snapshot.tracks));
 assert.equal(snapshot.missingParticipants.length,0,'the producer is not an expected recorder');
 const post=await fetch(`${origin}/api/v1/takes/${snapshot.take.id}/postprocess`,{method:'POST',headers:{Authorization:`Bearer ${session.hostToken}`}});
 assert.ok(post.ok,await post.text());
 const manifest=JSON.parse(await readFile(path.join(data,'sessions',session.id,'takes',snapshot.take.id,'out','manifest.json'),'utf8'));
 assert.deepEqual([...new Set(manifest.sources.map(s=>s.participant))].sort(),['Gia','Host'],'export contains host + guest only');
 console.log('take and export contain host + guest only');

 // The producer can't end the meeting or use any host route.
 const endWith=async tok=>(await fetch(`${origin}/api/v1/sessions/${session.id}/end`,{method:'POST',headers:{Authorization:`Bearer ${tok}`}})).status;
 assert.ok([401,403].includes(await endWith(ptoken)),'producer link ends the meeting');
 const grantStatus=await producer.evaluate(async({sid})=>{
  // The page holds its grant only in memory; it can still only reach what any participant can.
  const r=await fetch(`/api/v1/sessions/${sid}/end`,{method:'POST'});return r.status;
 },{sid:session.id});
 assert.ok([401,403].includes(grantStatus));
 for(const [method,p] of [['GET','/producer'],['POST','/producer/rotate'],['PATCH','/settings'],['POST','/invite'],['GET','/livestream']]){
  const r=await fetch(`${origin}/api/v1/sessions/${session.id}${p}`,{method,headers:{Authorization:`Bearer ${ptoken}`,'Content-Type':'application/json'},...(method==='PATCH'?{body:'{"recording_mic_mode":"raw"}'}:{})});
  assert.ok([401,403].includes(r.status),`${method} ${p} with the producer link: ${r.status}`);
 }
 const detail=await(await fetch(`${origin}/api/v1/sessions/${session.id}`,{headers:{Authorization:`Bearer ${session.hostToken}`}})).json();
 assert.ok(!detail.session.endedAt,'meeting still open');
 // Rotating the producer link refuses the old one at once (the admitted producer stays).
 const rotated=await(await fetch(`${origin}/api/v1/sessions/${session.id}/producer/rotate`,{method:'POST',headers:{Authorization:`Bearer ${session.hostToken}`}})).json();
 assert.ok(rotated.enabled&&!rotated.url.includes(ptoken));
 assert.equal((await fetch(`${origin}/api/v1/sessions/${session.id}`,{headers:{Authorization:`Bearer ${ptoken}`}})).status,401);
 assert.deepEqual(errors,[]);
 console.log('producer browser test passed');
}catch(err){console.error(err);console.error(logs.slice(-3000));process.exitCode=1;}
finally{await browser?.close();daemon.kill();}
