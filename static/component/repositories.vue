<div class="container">
    <div v-if="repositories.length == 0">
        <br><br>
        <p class="subtitle is-5">
            <router-link to="/new">{{ message.empty_repositories }}</router-link>
        </p>
    </div>
    <div class="field has-addons" v-else>
        <!-- <div class="field-label is-normal">
                <label class="label">From</label>
            </div> -->
            <p class="control" v-on:click="reverse = !reverse">
                <a class="button" v-if="reverse">
                    <span class="icon is-small">
                        <i class="fas fa-sort-amount-up" aria-hidden="true"></i>
                    </span>
                </a>
                <a class="button" v-else>
                    <span class="icon is-small">
                        <i class="fas fa-sort-amount-down" aria-hidden="true"></i>
                    </span>
                </a>
            </p>
            <p class="control">
                <span class="select">
                    <select>
                        <option value="name">{{ message.repo_name }}</option>
                        <option value="last_commit_time">{{ message.last_commit_time }}</option>
                        <option value="created_time">{{ message.created_time }}</option>
                    </select>
                </span>
            </p>
            <p class="control has-icons-left">
                <input class="input" type="text" placeholder="Search" v-model="keywords">
                <span class="icon is-small is-left">
                    <i class="fas fa-search"></i>
                </span>
            </p>
    </div>
    <div v-for="item, key in sorted" class="box repository-box">
        <article class="media">
            <div class="media-content">
                <div class="content">
                    <p>
                        <router-link v-bind:to="'/repository/' + item.name">
                            <strong>{{ item.name }}</strong>
                        </router-link>
                        <br>
                        {{ item.description }}
                    </p>
                </div>
                <!-- <nav class="level is-mobile">
                    <div class="level-left">
                        <a class="level-item" aria-label="reply">
                            <span class="icon is-small">
                                <i class="fas fa-reply" aria-hidden="true"></i>
                            </span>
                        </a>
                        <a class="level-item" aria-label="retweet">
                            <span class="icon is-small">
                                <i class="fas fa-retweet" aria-hidden="true"></i>
                            </span>
                        </a>
                        <a class="level-item" aria-label="like">
                            <span class="icon is-small">
                                <i class="fas fa-heart" aria-hidden="true"></i>
                            </span>
                        </a>
                    </div>
                </nav> -->
            </div>
        </article>
    </div>
</div>