package registry

import (
 "context"
 _ "embed"
 "encoding/json"
 "net/http"
 "strings"
 "io"
)

// Catalog metadata adapted from commandcode-api-proxy (MIT, copyright 2026 thaolaptrinh).
// See commandcode_LICENSE.
//go:embed commandcode_models.json
var commandCodeJSON []byte

type commandCodeCatalog struct {
 Builtin []string `json:"builtin"`
 Aliases map[string]string `json:"shortAliases"`
 Context map[string]int `json:"contextWindows"`
 Names map[string]string `json:"modelNames"`
 Efforts map[string][]string `json:"reasoningEfforts"`
 Output map[string]int `json:"maxOutputTokens"`
}

func commandCodeData() commandCodeCatalog {
 var c commandCodeCatalog
 _ = json.Unmarshal(commandCodeJSON, &c)
 return c
}

func ResolveCommandCodeModel(id string) string {
 id = strings.TrimPrefix(strings.TrimSpace(id), "commandcode/")
 c := commandCodeData()
 if canonical := c.Aliases[strings.ToLower(id)]; canonical != "" { return canonical }
 for _, canonical := range c.Builtin { if strings.EqualFold(id, canonical) || strings.EqualFold(id, canonical[strings.LastIndex(canonical,"/")+1:]) { return canonical } }
 return id
}

func CommandCodeEfforts(id string) []string { return commandCodeData().Efforts[ResolveCommandCodeModel(id)] }

func commandCodeStaticModels() []*ModelInfo {
 c := commandCodeData()
 out := make([]*ModelInfo,0,len(c.Builtin))
 for _, id := range c.Builtin {
 m := &ModelInfo{ID:id,Object:"model",OwnedBy:"commandcode",Type:"commandcode",DisplayName:c.Names[id],ContextLength:c.Context[id],MaxCompletionTokens:c.Output[id]}
 if levels := c.Efforts[id]; len(levels)>0 { m.Thinking=&ThinkingSupport{Levels:levels} }
 out=append(out,m)
 }
 return out
}

// FetchCommandCodeModels merges discovery metadata with the bundled fallback.
// Discovery is separate from generation, which always uses /alpha/generate.
func FetchCommandCodeModels(ctx context.Context, client *http.Client, base, key string) []*ModelInfo {
 models:=commandCodeStaticModels()
 if key=="" { return models }
 if base=="" {base="https://api.commandcode.ai"}
 req,err:=http.NewRequestWithContext(ctx,http.MethodGet,strings.TrimRight(base,"/")+"/provider/v1/models",nil)
 if err!=nil{return models}
 req.Header.Set("Authorization","Bearer "+key)
 resp,err:=client.Do(req); if err!=nil{return models}
 defer resp.Body.Close()
 if resp.StatusCode!=http.StatusOK{return models}
 var result struct {Data []struct { ID string `json:"id"`; Name string `json:"name"`; Context int `json:"context_length"` } `json:"data"`}
 if json.NewDecoder(io.LimitReader(resp.Body,4<<20)).Decode(&result)!=nil{return models}
 byID:=map[string]*ModelInfo{}
 for _,m:=range models{byID[m.ID]=m}
 for _,v:=range result.Data {
 if strings.TrimSpace(v.ID)=="" {continue}
 m:=byID[v.ID]
 if m==nil {m=&ModelInfo{ID:v.ID,Object:"model",OwnedBy:"commandcode",Type:"commandcode"};models=append(models,m);byID[v.ID]=m}
 if v.Name!=""{m.DisplayName=v.Name};if v.Context>0{m.ContextLength=v.Context}
 }
 return models
}
