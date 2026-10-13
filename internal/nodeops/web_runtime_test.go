package nodeops

import (
 "context"
 "os/exec"
 "strings"
 "testing"

 "pveforward/internal/proto"
)

type runtimeCapture struct { script string }
func (e *runtimeCapture) Run(_ context.Context, script string) ([]byte,error) { e.script=script; return []byte("##version\nopenresty/test\n"),nil }

func TestWebRuntimeInstall(t *testing.T) {
 for _, deploy := range []string{"binary", "docker"} {
  t.Run(deploy,func(t *testing.T) {
   ex:= &runtimeCapture{}
   _,err:=Web(context.Background(),ex,&proto.WebReq{Op:"install",Deploy:deploy,SHA256:strings.Repeat("a",64),Image:DefaultWebImage})
   if err!=nil {t.Fatal(err)}
   for _, want:=range []string{"trap rollback_install EXIT", "stop_current", "OLD_RT=$RT", "or_test \"$MAIN\"", "SWITCHED=1", "-previous"} {
    if !strings.Contains(ex.script,want) {t.Errorf("missing %q",want)}
   }
   if deploy=="binary" && strings.Index(ex.script,"SWAPPED=1") > strings.Index(ex.script,"mv "+BinPrefix+".new/pve-forward-openresty "+BinPrefix) { t.Fatal("rollback must be armed before swap") }
   if deploy=="docker" && strings.Index(ex.script,"docker pull -q") > strings.Index(ex.script,"  stop_current\n") {t.Fatal("must pull before stopping old runtime")}
   if bash,err:=exec.LookPath("bash");err==nil {
    cmd:=exec.Command(bash,"-n");cmd.Stdin=strings.NewReader(ex.script)
    if out,err:=cmd.CombinedOutput();err!=nil {t.Fatalf("script syntax: %v %s",err,out)}
   }
  })
 }
 for _,req:=range []*proto.WebReq{{Op:"install",Deploy:"auto"},{Op:"install",Deploy:"binary",SHA256:"bad"},{Op:"install",Deploy:"docker",Image:"x:y;id"}} {
  ex:= &runtimeCapture{}
  if _,err:=Web(context.Background(),ex,req);err==nil || ex.script!="" {t.Fatalf("invalid install executed: %+v",req)}
 }
}

func TestValidWebImage(t *testing.T) {
 for _,image:=range []string{DefaultWebImage,"registry.example.com:5000/team/openresty:v1","openresty/openresty@sha256:"+strings.Repeat("a",64)} {if !ValidWebImage(image){t.Errorf("rejected %s",image)}}
 for _,image:=range []string{"","openresty/openresty","https://example.com/x:y","x:y;id","x:y\n","-x:y","x@sha256:bad"} {if ValidWebImage(image){t.Errorf("accepted %q",image)}}
}
