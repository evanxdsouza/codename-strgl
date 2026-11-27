# Calls an OpenAI-compatible chat completions endpoint, such as a self-hosted
# llama.cpp `llama-server` or Ollama, or a hosted provider. Only
# StruggleRelabelRun uses it, and only with a minimised, redacted payload.
class StruggleRelabelClient
  class Error < StandardError; end

  Result = Data.define(:content, :input_tokens, :output_tokens)

  TIMEOUTS = { connect: 5, write: 30, read: 300 }.freeze

  def initialize(url:, model:, api_key: nil)
    @url = url.to_s.chomp("/")
    @model = model
    @api_key = api_key.presence
  end

  # schema constrains the answer where the server supports it; llama.cpp
  # turns it into a grammar, so even a small model returns valid JSON.
  def complete(system:, user:, max_tokens:, schema:)
    headers = { "Content-Type" => "application/json" }
    headers["Authorization"] = "Bearer #{@api_key}" if @api_key
    response = HTTP.timeout(**TIMEOUTS).headers(headers).post("#{@url}/chat/completions", json: {
      model: @model, temperature: 0, max_tokens:,
      response_format: { type: "json_schema", json_schema: { name: "labels", schema: } },
      messages: [ { role: "system", content: system }, { role: "user", content: user } ]
    })
    raise Error, "the model endpoint returned HTTP #{response.status.code}" unless response.status.success?

    body = JSON.parse(response.body.to_s)
    usage = body["usage"] || {}
    Result.new(
      content: body.dig("choices", 0, "message", "content").to_s,
      input_tokens: usage["prompt_tokens"], output_tokens: usage["completion_tokens"]
    )
  rescue HTTP::Error, JSON::ParserError => e
    raise Error, "the model endpoint failed: #{e.class.name}"
  end
end
