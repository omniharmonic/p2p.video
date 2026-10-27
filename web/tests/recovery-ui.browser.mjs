import assert from 'node:assert/strict';
import {fileURLToPath} from 'node:url';
import {createServer} from 'vite';
const {chromium}=await import(process.env.PLAYWRIGHT_MODULE||'playwright');
const server=await createServer({configFile:false,root:fileURLToPath(new URL('../',import.meta.url)),server:{host:'127.0.0.1',port:5191,strictPort:true}});
await server.listen(); let browser;
try {
  browser=await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH});
  const page=await browser.newPage(); await page.goto('http://127.0.0.1:5191');
  const result=await page.evaluate(async()=>{
    const {TransfersRecoveryModal}=await import('/src/components/TransfersRecoveryModal.ts');
    const {OnboardingWizard}=await import('/src/components/OnboardingWizard.ts');
    let serverMissing=false,missingRoster=false;
    window.fetch=async()=>Response.json({take:{id:'take',status:'stopped'},missingParticipants:missingRoster?[{id:'unrecorded'}]:[],tracks:[{trackId:'remote',kind:'audio',status:serverMissing?'recording':'assembled',totalBytes:2,participantName:'Remote'}]});
    let exports=0;
    const modal=new TransfersRecoveryModal('host-token',()=>{exports++});
    modal.setToken('host-token',true);
    modal.show({sessionTitle:'<img src=x onerror="window.injected=1">',takeId:'take',takeNumber:1,durationSec:2,participantsCount:2,
      tracks:[{trackId:'local',participantName:'<svg onload="window.injected=1">',sourceLabel:'<img src=x>',kind:'video',localBytes:2,serverBytes:2,status:'verified',lastSeq:0}]});
    await new Promise(resolve=>setTimeout(resolve,50));
    const injection=document.querySelector('#transfers-recovery-modal img, #transfers-recovery-modal svg')!==null;
    const wasEnabled=!document.querySelector('#prepare-export-btn').disabled;
    serverMissing=true;
    const button=document.querySelector('#prepare-export-btn'); button.click(); await new Promise(resolve=>setTimeout(resolve,50));
    const staleDisabled=document.querySelector('#prepare-export-btn').disabled;
    serverMissing=false;missingRoster=true;await modal.refreshFromHost();
    const rosterDisabled=document.querySelector('#prepare-export-btn').disabled;
    const wizard=new OnboardingWizard({onComplete(){},async onRequestPermissions(){return true;}}); wizard.show();
    const promises=document.querySelector('#onboarding-wizard-overlay').textContent;
    const {OPFSRecoveryBanner}=await import('/src/components/OPFSRecoveryBanner.ts');
    const {OPFSStorage}=await import('/src/lib/opfs.ts');
    const storage=OPFSStorage.getInstance();
    for(const trackId of ['one','two']) await storage.saveMeta('take',trackId,{takeId:'take',trackId,uploadToken:'token',kind:'video',lastSeq:0,isComplete:false,createdAt:1});
    const banner=new OPFSRecoveryBanner(''); await banner.checkAndMount(document.body);
    document.querySelector('#resume-opfs-btn').click();
    await new Promise(resolve=>setTimeout(resolve,100));
    const recoveryText=document.querySelector('#opfs-recovery-banner').textContent;
    return {injection,exports,wasEnabled,staleDisabled,rosterDisabled,disabled:document.querySelector('#prepare-export-btn').disabled,promises,recoveryText};
  });
  assert.equal(result.injection,false); assert.equal(result.exports,0); assert.equal(result.wasEnabled,true); assert.equal(result.staleDisabled,true);assert.equal(result.rosterDisabled,true); assert.equal(result.disabled,true);
  assert.match(result.recoveryText,/2 still to upload/);
  assert.doesNotMatch(result.promises,/uncompressed|sub-millisecond|strictly over.*WireGuard|preventing data loss/i);
  console.log('Recovery UI injection, authoritative export gate and truthful setup assertions passed');
} finally {await browser?.close();await server.close();}
