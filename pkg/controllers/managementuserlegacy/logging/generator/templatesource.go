package generator

var SourceTemplate = `
{{define "source-rke"}}
<source>
  @type  tail
  path  /var/lib/rancher/rke/log/*.log
  pos_file  /fluentd/log/{{ .RkeLogPosFilename }}
  time_format  %Y-%m-%dT%H:%M:%S.%N
  tag  {{ .RkeLogTag }}.*
  format  json
</source>
{{end}}

{{define "source-container"}}
<source>
  @type  tail
  path  /var/log/containers/*.log
  pos_file  /fluentd/log/{{ .ContainerLogPosFilename}}
  tag  {{ .ContainerLogSourceTag }}.*
  skip_refresh_on_startup true
  read_from_head true

  <parse>
	@type multi_format
	<pattern>
	  format json
	  time_format %Y-%m-%dT%H:%M:%S.%NZ
	</pattern>
	<pattern>
	  format regexp
	  time_format %Y-%m-%dT%H:%M:%S.%N%:z
	  expression /^(?<time>.+)\b(?<stream>stdout|stderr)\b(?<log>.*)$/
	</pattern>
  </parse>
</source>
{{end}}

{{define "source-project-container"}}
<source>
  @type  tail
  path  {{ .ContainerSourcePath}}
  pos_file  /fluentd/log/{{ .ContainerLogPosFilename}}
  time_format  %Y-%m-%dT%H:%M:%S.%N
  tag  {{ .ContainerLogSourceTag }}.*
  format  json
  skip_refresh_on_startup true
  read_from_head true
</source>
{{end}}
`
