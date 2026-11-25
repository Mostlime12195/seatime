# How SeaTime counts your time

Rule `heartbeat/1`.

## The rule

**SeaTime counts hours the way Hackatime does: heartbeats, with every gap between two heartbeats counted up to 2 minutes. You make a heartbeat with a hand edit or a new prompt; an agent working on your prompt makes one a minute, and the agent's count only in the 10 minutes after one of yours.**

The counting is Hackatime's own. It is taken from Hackatime's source and not changed. SeaTime adds one thing: it decides which AI heartbeats count.

## The definitions

1. **Your heartbeats.** A heartbeat that your editor's WakaTime plugin reports as `coding`, or a new prompt to an AI tool.
2. **New prompt.** A prompt of 4 words or more that you have not already sent in this project. SeaTime ignores capital letters and punctuation when it compares prompts. "yes", "continue", "fix it" and a prompt that you sent before are not heartbeats of yours. They are not marked down. They just do not start the 10 minutes.
   While the AI check is on, definitions 2 and 3 work differently. See "When the AI check is on" below.
3. **The agent's heartbeats.** They count in the 10 minutes after a heartbeat of yours in the same project. There are three kinds:
   - one heartbeat a minute, from your prompt until the agent stops, when SeaTime hears from the AI tool;
   - any heartbeat that your editor's plugin reports as `ai coding`. The plugin marks any edit that an AI made, whatever tool made it, so SeaTime does not need to know the tool;
   - editor heartbeats that arrive while an agent that SeaTime hears from is running in the same project. An agent that edits an open file makes the same heartbeat that you do.
4. **Project.** A gap belongs to the project of the heartbeat that ends it. Hackatime's count for each user works the same way. The heartbeats from all of your devices and tools go in one list in time order, so that a minute is never counted twice.
   - A hand edit belongs to the project of the file, as on Hackatime.
   - An AI session belongs to one project: the one that it was started in, which is the git repo of the folder where you started the AI tool. Every prompt and everything the agent does in that session counts there. If the agent changes folder or writes a file somewhere else, the session stays where it is and no new project appears. Such a file is recorded as a file outside the project.
   - A session started outside any repo goes by where it writes: a repo inside the folder that it was started in, or else that folder. A session started in your home folder goes to the repo or folder that it writes in. An AI tool's own folders, such as `~/.claude`, are never a project.

The rule has three numbers: 2 minutes (from Hackatime), 10 minutes and 4 words. It has no weights, caps, discounts or scores.

## When the AI check is on

The server can turn on an AI check. Each account can also turn it off for itself in Settings. It is on unless you turn it off. While it is off, everything above holds as written. While it is on, the points below replace definitions 2 and 3. The rule that applies depends on when the work happened. Time from while the check was off counts as above. Time from while it was on keeps counting this way, even after the check is turned off. If a stretch of records spans a change, each side is counted by its own rule.

- **While your setting is off.** Your prompts are still saved on your computer. A hash of each one goes to the server right away, so that any later change to a prompt can be seen. Your time counts as above, without the check.
- **When you turn it back on.** The prompts from while it was off are checked too. Once all of them have an answer, that time counts by the check, like the rest. Until then, it counts as above.
- **When an application reads your project.** You may have connected an application, such as the site of a Hack Club YSWS program. The first time that it reads a project, the server asks your computer for the prompts that it needs and checks them. This happens whatever your setting is. A prompt whose text does not match its hash counts as not passing. So does a prompt that has not reached the server 14 days after the server asked for it.

- **Which prompts count.** An AI model reads every prompt that you have not sent before in this project. Short prompts are read too. With each prompt, the model also reads the AI's reply before it, the code change after it, the project's turns before it and the project's earlier prompts that counted. It also reads what the project is: its name, some of its file names and the start of its README, as they were when you sent the prompt. Turns from any chat are included: starting a new chat does not hide them. A prompt counts when the model finds that it adds something of yours to the project that you are building. The questions that the model answers are not public, so that nobody can write prompts to fit them.
- **How long a prompt counts.** A prompt that counts does so for up to 15 minutes from when you sent it, depending on how good it is. It stops sooner at your next prompt. It also stops sooner if the AI is done and you then leave the keyboard and mouse alone for 2 minutes. In that case it counts up to your last input.
- **The agent's own time** does not count by itself. Hand edits count as they do with the check off. A minute is never counted twice.
- **Who counts it.** The server does. Your computer sends each prompt for its check and is told only that the prompt was checked. It is not told whether the prompt counts or for how long, so it counts your hand time and no AI time. Once a stretch of work is over, your computer sends its records, about 3 hours after you stop. The server then counts the time of the prompts in them, and that time appears on the site. Until then, your profile shows your hand time and how many prompts are not checked yet.
- **Waiting for the check.** Sometimes the server cannot check a prompt for a while, for example during an outage or when its daily budget runs out. Then the prompt keeps waiting on your computer for as long as it takes. Nothing is decided or lost in the meantime.

The minutes are a server setting. Each checked prompt keeps the minutes that it was given. So changing the setting never changes time that was already counted.

| What happened | Hours |
|---|---|
| A prompt at 10:00 that the check gives 10 minutes; the agent works until 10:30 | 10 min; 20 min of AI time not counted |
| A prompt at 10:00 that passes; your next prompt at 10:04 | 4 min for the first prompt |
| A prompt at 10:00 that passes; the agent is done at 10:02; you read until 10:03, then nothing for 5 minutes | 3 min |
| "yes" at 10:00, the first time you say it in the project | it is checked; it adds nothing of yours, so 0 min |
| A prompt the check does not pass at 10:00; hand edits at 10:05, 10:06 and 10:07 | 2 min (the hand edits, as before) |
| A prompt at 10:00 that the check gives 10 minutes; hand edits at 10:03 and 10:12 | 10 + 2 = 12 min |

## Time by model

SeaTime also splits the counted time by AI model. This is a breakdown of the hours above, and it changes no number.

- A prompt's time goes to the model that answered it, under the name that the AI tool reports. This is the model named on the last record of the prompt's turn, such as the agent's stop. If none of those records names a model, it is the model named on the prompt.
- An edit made while an agent is running counts as the agent's, so it goes to that agent's model.
- Time from hand edits has no model.
- AI time is "not recorded" when no record names a model. This is the case for a tool that does not report its model (Gemini CLI), and for time from before SeaTime kept the model.

You see the split on your own profile and project pages. An application that you connected reads it for a project.

## How this is "productive struggle"

This section describes the rule with the AI check off. With the AI check on, the minutes are those of "When the AI check is on" above, and the agent's time never counts by itself.

- **Hand work** counts exactly as it does on Hackatime today.
- **AI work counts in full while you are steering it.** If the agent is working on something new that you said in the last 10 minutes, its time is your time. A project that AI wrote 100% of, but that you steered closely, gets nearly every minute.
- **AI work stops counting when you stop steering.** Say the agent keeps running after you left, or you only feed it "yes", "continue" and "fix it". Then none of its time counts after the 10 minutes run out.
- **What the rule does not judge:** whether the output works, whether a new prompt was a good one, and whether the work was easy for you.

## An hour counted should mean what a Hackatime hour means

The bar is Hackatime for hand coding during Hack Club High Seas. People trusted a Hackatime hour then for two reasons. SeaTime keeps both.

- **Every counted minute follows something that you did.** Then it was a keystroke in the last 2 minutes. Now it is a hand edit in the last 2 minutes or a new prompt in the last 10.
- **Nobody declares AI use.** SeaTime never asks how much AI you used. AI work that SeaTime hears from is counted by this rule. AI work that it does not hear from is not counted. It is never counted as hand work because you left a box empty.

## Worked examples

| What happened | Hours |
|---|---|
| Hand edits at 10:00, 10:01 and 10:05 | 1 + 2 = 3 min, the same as Hackatime |
| New prompt at 10:00, agent works until 10:08, new prompt at 10:12 | 8 + 2 = 10 min |
| A new prompt every 12 minutes for 2 hours (10 prompts), each run 9 minutes | 9 × 11 + 9 = 108 min of 117 |
| New prompt at 10:00, agent works until 11:00, new prompt at 11:00 | 10 + 2 = 12 min; 50 min of AI time not counted |
| New prompt at 10:00, then "yes" at 10:04, 10:08 and 10:12, agent working until 10:15 | 10 min |
| New prompts at 10:00, 11:00 and 12:00, each followed by a 55-minute run | 10 + 12 + 12 = 34 min; 135 min not counted |
| Project A at 14:00, B at 14:01, A at 14:02 | B: 1 min, A: 1 min |
| Hand edit at 10:00. An AI tool SeaTime does not hear edits an open file every minute until 10:30, and the plugin reports `ai coding` | 10 min; 20 min of AI time not counted |
| The same, with no hand edit | 0 min; 30 min of AI time not counted |

## Known limits

This section describes the rule with the AI check off. With the AI check on, the minutes are those of "When the AI check is on" above, and the agent's time never counts by itself.

- A script that sends fake heartbeats or fresh prompts is counted like a person, exactly as on Hackatime.
- A language model that types fresh prompts into a real desktop is counted in full.
- A person who sends a real prompt and walks away gets up to 12 minutes for it. A person who sends a fresh prompt every 10 minutes and does nothing else is counted in full.
- A person who is present and sends different low-effort prompts is counted in full.
- Thinking or reading for more than 2 minutes without a heartbeat is not counted. That is Hackatime's behaviour and SeaTime keeps it.
- If you hand edit while your own agent is running in that project, those edits count as the agent's.
- An AI tool that SeaTime hears from only through the editor (edits, but no prompts) counts for 10 minutes after each hand edit and no longer. `seatime status` shows when this is happening.
- An AI tool that never touches this computer (an agent that runs in the cloud or from a phone) is not seen. Its work arrives as commits with no hours.
- If an editor plugin does not mark an AI's edit as `ai coding` and SeaTime does not hear from the tool, the edit counts as yours.
- Watching a run longer than 10 minutes without saying anything new stops counting at 10.
- Time at a workbench, or testing your app by hand, is not seen at all.
