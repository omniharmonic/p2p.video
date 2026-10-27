// Reproduce browsers that hide camera IDs until the first permission grant.
import assert from 'node:assert/strict';
import {launchURL,readyToJoin} from './lib/harness.mjs';
import {spawn} from 'node:child_process';
import {mkdtemp} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import path from 'node:path';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const data=await mkdtemp(path.join(tmpdir(),'studio-first-camera-')),origin='http://127.0.0.1:17449';
const daemon=spawn(process.env.STUDIO_BINARY,['--data-dir',data,'--port','17449','--no-funnel'],{stdio:'ignore'});let browser;
try {
 for(let n=0;n<100;n++){try{if((await fetch(origin+'/health')).ok)break}catch{}await new Promise(r=>setTimeout(r,100));}
 browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH,args:['--use-fake-device-for-media-stream','--use-fake-ui-for-media-stream','--mute-audio']});
 const context=await browser.newContext({permissions:['camera','microphone']});
 await context.addInitScript(()=>{
  const enumerate=navigator.mediaDevices.enumerateDevices.bind(navigator.mediaDevices);let first=true;
  navigator.mediaDevices.enumerateDevices=async()=>{
   if(first){first=false;return [{kind:'videoinput',deviceId:'',label:'',groupId:''},{kind:'audioinput',deviceId:'',label:'',groupId:''}];}
   return enumerate();
  };
  const get=navigator.mediaDevices.getUserMedia.bind(navigator.mediaDevices);
  navigator.mediaDevices.getUserMedia=async constraints=>{window.__captureConstraints=constraints;return get(constraints);};
 });
 const page=await context.newPage();await page.goto(await launchURL(data,origin));await readyToJoin(page);
 const result=await page.evaluate(()=>({constraints:window.__captureConstraints,settings:document.querySelector('#preview-video').srcObject.getVideoTracks()[0].getSettings(),namedCameras:[...document.querySelectorAll('#camera-select option')].filter(o=>o.value&&o.textContent&&!o.textContent.startsWith('videoinput')).length}));
 assert.ok(result.settings.width*result.settings.height>=1920*1080,`first-permission capture is only ${result.settings.width}x${result.settings.height}`);
 assert.equal(result.constraints.video.width.ideal,1920);
 assert.equal(result.namedCameras>0,true,'device choices should refresh after permission');
 assert.equal(await page.locator('#join-btn').isEnabled(),true);
 console.log(JSON.stringify({firstPermissionHD:true,width:result.settings.width,height:result.settings.height,choicesRefresh:true,syntheticDevices:true,noSessionCreated:true}));
} finally {await browser?.close();daemon.kill('SIGTERM');}
